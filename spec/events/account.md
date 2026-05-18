# account events — position 生命周期

本文档描述 `x/account` 模块发射的 **Position 生命周期事件**（issue #91）。这些事件
是 indexer 重建 `AccountPosition` 表与单条仓位生命线（lifeline）的唯一可靠来源。

> 与 `EventAccountUpdated` / `EventAccountAssetUpdated` 一同共同构成 `x/account`
> 模块的 **state-change 事件层**：每一次 keeper 写入都产生一条事件，indexer 仅靠
> 消费事件流即可以 last-write-wins 语义重建 `Accounts` / `AccountAssets` /
> `AccountPositions` 三张规范表。

---

## 设计动机

历史实现里 `x/account.Keeper.UpdatePosition` 同时承担 **开仓 / 加仓 / 减仓 /
平仓 / 反向开仓 / 杠杆设置** 多种语义，统一发射一条 `EventPositionUpdated`。
这导致 indexer 必须对比新旧 `base_size` 才能判断仓位边界，并且：

1. 缺乏唯一的 `position_id`：indexer 无法把 fills / funding / 调仓事件 join 回
   "这是一段独立的仓位生命周期"。
2. 完全平仓后存储不释放：`AccountPosition` 行被保留为零仓位行，长期占用 KV
   存储空间，且与"被取消的订单也应该从 store 移除"的理念不一致。

新设计把单一事件拆成 **三个语义清晰的事件 + 一个 `position_id` 主键 + 自动
回收存储** 三件事一起做。

---

## 数据模型变更

### `AccountPosition.position_id`

新增 `uint64 position_id = 12;`（详见 `proto/perpdex/account/v1/account.proto`）。

`position_id` 由 `x/account.Keeper.NextPositionIndex` 分配，**全局单调递增、跨
account / market 唯一**。indexer 可以把它作为仓位生命线的主键。

| 行状态                               | `position_id` |
|--------------------------------------|---------------|
| Open 中（`base_size != 0`）          | 非零，开仓时一次性分配，整个生命期不变 |
| Closed 后被回收（行已删除）          | 行不存在 |
| Closed 但保留为杠杆配置行            | 0（重置） |
| Leverage-only 配置行（从未开仓）     | 0 |

> `position_id == 0` 是"非开仓状态"的哨兵；任何 `base_size != 0` 的行 **必须**
> 携带非零 `position_id`，反之亦然。InitGenesis 的 `Validate()` 会强制该不变量。

### `Counters.next_position_index`

`GenesisState.Counters` 新增 `uint64 next_position_index = 3;`，用于 export /
import 的 round-trip。Genesis Validate 强制
`next_position_index > max(persisted position_id)`。

### 存储回收语义

完全平仓时（`base_size: !=0 → 0`），生命周期 dispatcher 按"杠杆是否为默认"决定
是否回收 KV 行：

- **默认杠杆**（`margin_mode == Cross && initial_margin_fraction == 0`）：直接
  `AccountPositions.Remove(...)`，释放 KV 空间，事件 `deleted = true`。
- **非默认杠杆**：保留行但把 `base_size` / `entry_quote` /
  `last_funding_rate_prefix_sum` / `allocated_margin` / `position_id` /
  `total_position_tied_order_count` / `created_at` 全部重置为零，仅保留
  `margin_mode` / `initial_margin_fraction`，事件 `deleted = false`。

这样用户配置过的杠杆偏好能跨"平仓 → 再开仓"周期存活，同时绝大多数交叉默认杠
杆的用户依然享受存储自动回收。

---

## 事件

### EventPositionOpened

在 `base_size` 从 `0` 跨入非零时（含 flip 的 open 半幕）发射。携带 **刚分配的
`position_id`** 与开仓后的完整快照。

```proto
message EventPositionOpened {
  AccountPosition position = 1;
}
```

**触发场景**

- 普通开仓：用户通过 `MsgCreateOrder` 撮合产生 fill，`x/trade.applyPositionChange`
  把 `base_size` 从 0 推到非零。
- Flip 的 open 半幕：原仓位反向 fill 越过零点（先发 `EventPositionClosed`
  再发 `EventPositionOpened`）。
- Liquidation / ADL 接管方（LLP / IF / 高杠杆账户）首次承接仓位。

**Invariants**

- `position.position_id != 0`，且全网历史 ID 唯一。
- `position.base_size != 0`。
- `position.created_at` 等于该事件所在块的 `BlockTime` 毫秒（除非 caller 显式预
  填）。
- 紧邻这条事件的 `EventOrderFilled` 中相应一侧的 `*_position_id` 等于
  `position.position_id`（taker 与 maker 各自独立判断）。

---

### EventPositionUpdated

任何对 **已经存在的非零仓位** 或 **杠杆配置行** 的就地写入都会发射本事件。
`position_id` 保持不变（更新分支）或为 0（杠杆配置行分支）。

```proto
message EventPositionUpdated {
  AccountPosition position = 1;
}
```

**触发场景**

- 同向加仓 / 同向减仓但未平仓（`base_size` 同号且未归零）。
- `x/funding.SettlePositionFunding` 把累积资金费率折入 `entry_quote`。
- `x/account` 的 `UpdateMargin`（隔离仓位增减保证金）/ `UpdateLeverage`
  （`SetPositionLeverage` 写入杠杆配置行）。
- `x/liquidation` 不直接调用，但任何未来要走 keeper 写路径的清算修补也会落进
  这条事件。
- isolated margin 的 `applyIsolatedAccount` / `rebalanceIsolatedMargin` 调整
  `allocated_margin`。

**Invariants**

- 真实仓位更新：`position.position_id` 等于上一次该 `(account, market)` 的
  `EventPositionOpened.position.position_id`。
- 杠杆配置写入：`position.position_id == 0` 且 `position.base_size == 0`，至少
  一个 `margin_mode` / `initial_margin_fraction` 字段非默认。
- 同向更新不会改变 `base_size` 的符号；side flip 走 Closed + Opened 两条事件而
  非 Updated。

---

### EventPositionClosed

`base_size` 从非零归零时（含 flip 的 close 半幕）发射。携带 **被关闭的
`position_id`** 与平仓瞬间的快照（`base_size = 0`），并通过 `deleted` 标记底层
KV 行是否真的被删除。

```proto
message EventPositionClosed {
  AccountPosition position = 1;
  // 若底层行被回收（默认杠杆路径）则为 true；若保留为 leverage-only 配置行
  // （非默认杠杆路径）则为 false。
  bool deleted = 2;
}
```

**触发场景**

- 普通完全平仓：用户反向单击中 fill 使得 `base_size` 归零。
- Flip 的 close 半幕：反向 fill 超过当前仓位规模，先关闭原仓位再开新仓位。
- 清算 / ADL 把仓位整体吃掉。
- Market expiry exit / IF 强制平仓。

**Invariants**

- `position.position_id != 0`（重置发生在事件之后；事件 payload 保留旧 ID）。
- `position.base_size.IsZero() == true`。
- `deleted = true` ⇔ 该行从 `AccountPositions` 中被 `Remove`；
  `deleted = false` ⇔ 该行被保留为 leverage-only 配置行
  （`base_size = 0, position_id = 0`，仅留杠杆字段）。
- 紧邻这条事件的 `EventOrderFilled` 中相应一侧的 `*_position_id` 等于
  `position.position_id`（"完全平仓时沿用旧 ID"——和 `matching.md` 中
  `EventOrderFilled` 的描述一致）。

---

## 生命周期转移图

```
                       ┌────────────────────────┐
       (uninit)        │  leverage-only config  │
   row 不存在 ─────────▶  base_size=0,           │
                       │  position_id=0,         │
                       │  margin_mode != Cross   │
                       │  或 imf != 0            │
                       └────────────┬───────────┘
        │  open          │  Updated (leverage 写入)
        │  (Opened, new id)
        ▼                ▼
    ┌─────────────────────┐   Updated  ┌─────────────────────┐
    │  Open               │ ◀───────── │  Open（同向 size 变 ）│
    │  base_size != 0     │            │                     │
    │  position_id = N    │  Updated   │                     │
    └────┬───────────┬────┘ ─────────▶ └─────────────────────┘
         │ close     │  flip
         │ (Closed)  │  (Closed old id + Opened new id)
         ▼           ▼
   ┌──────────────────────┐
   │ row 被回收（deleted)  │
   │ 或保留为 leverage-only│
   └──────────────────────┘
```

---

## indexer 处理建议

1. 维护一张 `positions(position_id PK, account_index, market_index, ...)` 表，
   按 `position_id` 主键写入。
2. `EventPositionOpened`：`INSERT` 新行；`opened_at = block_time`。
3. `EventPositionUpdated`（`position_id != 0`）：`UPDATE` 主键行；
   `last_update_at = block_time`。
4. `EventPositionUpdated`（`position_id == 0`）：杠杆配置行变更，可以另存
   `(account_index, market_index) → margin_mode, imf` 配置表，与仓位主表分开。
5. `EventPositionClosed`：把 `positions[position_id]` 标记 `closed_at = block_time`，
   保留历史（不要物理删除——`deleted = true` 仅描述链上 KV 的回收行为，
   indexer 是另一份独立账本）。
6. 关联事件：`EventOrderFilled.taker_position_id` / `maker_position_id` 是
   关联 fill → position 的 join key；spot fill 为 0。

---

## 兼容性 / 迁移说明

- `EventPositionUpdated` 仍然存在，但语义收窄到"已有非零仓位的同向更新 + 杠杆
  配置写入"。升级前依赖该事件区分开仓 / 平仓的 indexer 实现需要切换到新事件。
- 新增的 `position_id` 字段不会出现在升级前的 fills / 仓位事件里；indexer 建议
  以 `position_id != 0` 作为 "采用新协议" 的判定。
- Genesis 字段 `Counters.next_position_index` 是新增的，升级前的快照默认值 0，
  由 `InitGenesis` 自动 coerce 到 `max(persisted position_id) + 1`，所以无需迁
  移脚本。
- `AccountPosition` 行的物理回收对 `IterateAccountPositions` 的调用方透明：仍
  然只会迭代已存在的行，调用方依然应当用 `pos.BaseSize.IsZero()` 跳过 leverage-
  only 配置行（与升级前同语义）。
