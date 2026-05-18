# account events — position 生命周期

本文档描述 `x/account` 模块发射的 **Position 生命周期事件**（issue #91）。这些事件
是 indexer 重建 `AccountPosition` 表与单条仓位生命线（lifeline）的唯一可靠来源。

> 与 `EventAccountUpdated` / `EventAccountAssetUpdated` 一同共同构成 `x/account`
> 模块的 **state-change 事件层**：每一次 keeper 写入都产生一条事件，indexer 仅靠
> 消费事件流即可以 last-write-wins 语义重建 `Accounts` / `AccountAssets` /
> `AccountPositions` 三张规范表。

---

## 设计动机

历史实现里 `x/account.Keeper.UpdatePosition` 是一个万能的 RMW 入口，同时承担
**开仓 / 加仓 / 减仓 / 平仓 / 反向开仓 / 杠杆设置** 多种语义，并统一发射一条
`EventPositionUpdated`。这种"一招吃遍天"的 API 形态既让 caller 的意图变得不
显式，也让 indexer 必须对比新旧 `base_size` 才能判断仓位边界。具体痛点：

1. **API 边界模糊**：caller 是开仓、加减仓、平仓还是只调杠杆，从签名上看不出来；
   一个 closure 内的逻辑错误（例如不小心把 base_size 写成 0）会被"宽容"地走完
   close 分支，而不是显式失败。
2. **缺乏唯一 `position_id`**：indexer 无法把 fills / funding / 调仓事件 join 回
   "这是一段独立的仓位生命周期"。
3. **完全平仓后存储不释放**：`AccountPosition` 行被保留为零仓位行，长期占用 KV
   存储空间，与"被取消的订单也应该从 store 移除"的理念不一致。

新设计 (issue #91) 把 `UpdatePosition` **完全移除**，替换为三个语义窄、前后置
不变量明确的 keeper 方法 —— `OpenPosition` / `MutatePosition` /
`ClosePosition` —— 配合一个 `position_id` 主键与自动回收存储一起落地。

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

## Keeper API

`x/account.Keeper` 暴露 **三个显式的仓位生命周期方法**，每个方法只对应一种状态
转移，并强制前后置不变量；任何违反不变量的 caller 都会以
`ErrPositionLifecycleViolation` 失败：

| 方法 | 前置 (`pre`) | 后置 (`post`) | 副作用 |
|------|--------------|--------------|--------|
| `OpenPosition(acc, mkt, mut)` | `pre.BaseSize == 0` | `post.BaseSize != 0` | 分配 `position_id`、stamp `CreatedAt`、发射 `EventPositionOpened` |
| `MutatePosition(acc, mkt, mut)` | `pre.BaseSize != 0` | `post.BaseSize != 0` 且 `sign(post) == sign(pre)` | 保留 `position_id`、发射 `EventPositionUpdated` |
| `ClosePosition(acc, mkt)` | `pre.BaseSize != 0` | — | 删除 KV 行（或保留为 leverage-only 配置行），发射 `EventPositionClosed`；**返回 pre-close 快照**给 caller 用作下游 reconciliation |

外加一个用于杠杆配置的方法（不属于生命周期，但同样会发事件给 indexer）：

| 方法 | 用途 | 事件 |
|------|------|------|
| `SetPositionLeverage(acc, mkt, mode, imf)` | 写一个 `BaseSize == 0, position_id == 0` 的 leverage-only 配置行 | `EventPositionUpdated`（`position_id == 0`） |

### Flip 由 caller 显式编排

`x/trade.applyPositionChange` 在 `ApplyFill` 计算出 fill 越过零点（`fill.SideFlipped`）后，**显式** 地先 `ClosePosition` 再 `OpenPosition`，并把
`ClosePosition` 返回的 pre-close `AllocatedMargin` / `LastFundingRatePrefixSum`
通过 OpenPosition 的 mut 闭包搬到新仓位上，保留 issue #91 之前 isolated margin
flip 路径的 re-margin 语义。Indexer 看到的序列就是：

```
EventPositionClosed { position_id = OLD }
EventPositionOpened { position_id = NEW }
```

两条事件落在同一笔交易里、有严格顺序。

### Caller cheat-sheet

| 场景 | 调用顺序 |
|------|----------|
| 普通开仓 | `OpenPosition` |
| 同向加减仓 | `MutatePosition`（mut 只改 `BaseSize` / `EntryQuote` / `AllocatedMargin` / `LastFundingRatePrefixSum`） |
| 资金费率结算（已开仓） | `MutatePosition`（折入 `EntryQuote` + 更新快照） |
| 资金费率结算（未开仓） | 直接 short-circuit，不写入；下次 `OpenPosition` 从市场当前 prefix 重新 seed |
| Isolated margin 调仓 | `MutatePosition`（调 `AllocatedMargin`） |
| 完全平仓 | `ClosePosition`；isolated caller 自行把 `pre.AllocatedMargin + PnL - fee` 加回 cross collateral |
| Flip | `ClosePosition` + `OpenPosition`（caller 串联） |
| 改杠杆 | `SetPositionLeverage`（仅 `BaseSize == 0` 时可用） |
| Genesis 恢复 | `setPosition`（package-private，跳过事件与 ID 分配） |

---

## 事件

### EventPositionOpened

由 `Keeper.OpenPosition` 在写完新仓位行之后发射。携带 **刚分配的 `position_id`**
与开仓后的完整快照。

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

由两个方法发射：

- `Keeper.MutatePosition`（同向加减仓 / funding 折入 / isolated margin 调
  整 / `UpdateMargin`）—— 携带稳定的非零 `position_id`。
- `Keeper.SetPositionLeverage` —— 携带 `position_id == 0`，标识这是一个
  leverage-only 配置写入而非真实仓位更新。

```proto
message EventPositionUpdated {
  AccountPosition position = 1;
}
```

**触发场景**

- 同向加仓 / 同向减仓但未平仓（`base_size` 同号且未归零）。
- `x/funding.SettlePositionFunding` 把累积资金费率折入 `entry_quote`（仅在
  仓位已开仓时；空仓位会被 short-circuit）。
- `x/account` 的 `MsgUpdateMargin`（隔离仓位增减保证金，要求 `base_size != 0`）。
- `x/trade` isolated margin 的 `applyIsolatedAccount` /
  `rebalanceIsolatedMargin` 调整 `allocated_margin`（仅在仓位仍开仓的非
  close 分支；close 分支直接 `AddCollateral`，不发 Updated）。
- `x/account.SetPositionLeverage` 写入 leverage-only 配置行（`MsgUpdateLeverage`
  / `MsgUpdateMargin` 的 margin-mode 调整路径，要求 `base_size == 0`）。

**Invariants**

- 真实仓位更新（`MutatePosition`）：`position.position_id` 等于上一次该
  `(account, market)` 的 `EventPositionOpened.position.position_id`，
  `position.base_size` 与上一次同号且不为零。
- 杠杆配置写入（`SetPositionLeverage`）：`position.position_id == 0` 且
  `position.base_size == 0`，至少一个 `margin_mode` /
  `initial_margin_fraction` 字段非默认。
- side flip **不会** 发 Updated；它走 `ClosePosition + OpenPosition` 两条事件。

---

### EventPositionClosed

由 `Keeper.ClosePosition` 发射。携带 **被关闭的 `position_id`** 与平仓后的快照
（`base_size = 0, entry_quote = 0`），并通过 `deleted` 标记底层 KV 行是否真的
被删除。

```proto
message EventPositionClosed {
  AccountPosition position = 1;
  // 若底层行被回收（默认杠杆路径）则为 true；若保留为 leverage-only 配置行
  // （非默认杠杆路径）则为 false。
  bool deleted = 2;
}
```

**触发场景**

- 普通完全平仓：`x/trade.applyPositionChange` 检测到 `fill.Position.BaseSize == 0`，
  调用 `ClosePosition`。
- Flip 的 close 半幕：`fill.SideFlipped == true`，caller 先 `ClosePosition` 再
  `OpenPosition`。
- 清算 / ADL 把仓位整体吃掉（沿用与普通平仓相同的 trade engine 路径）。
- Market expiry exit / IF 强制平仓。

**Invariants**

- `position.position_id != 0`（pre-close 的真实 ID，indexer 用它锁定生命线）。
- `position.base_size.IsZero() == true`，`position.entry_quote.IsZero() == true`。
- `deleted == true` ⇔ 该行从 `AccountPositions` 中被 `Remove`；
  `deleted == false` ⇔ 该行被保留为 leverage-only 配置行
  （`base_size = 0, position_id = 0`，仅留杠杆字段）。
- 紧邻这条事件的 `EventOrderFilled` 中相应一侧的 `*_position_id` 等于
  `position.position_id`（"完全平仓时沿用旧 ID"——和 `matching.md` 中
  `EventOrderFilled` 的描述一致）。

**Caller 侧的注意事项**

`Keeper.ClosePosition` 返回 **pre-close 快照**（`BaseSize`、`AllocatedMargin`
等仍为 close 之前的值），用途是让 caller 自行处理下游 reconciliation：

- Cross margin：直接走 `applyCrossAccount`，把 `realized_pnl - fee` 加到 cross
  collateral 即可，不需要碰 pre-close 字段。
- Isolated margin：`applyIsolatedAccount` 在 close 分支不再走
  `MutatePosition`（仓位已不存在），而是把
  `pre.AllocatedMargin + realized_pnl - fee - liq_fee` 一次性 `AddCollateral` 回
  cross collateral，等价于旧版本"add → rebalance → drain"三步串联，但少了两次
  写入和两条 Updated 事件。

---

## 生命周期转移图

```
                                  ┌─────────────────────────┐
        (uninit)                  │   leverage-only config  │
   row 不存在 / 默认  ◀──────────▶ │   base_size = 0          │
                                  │   position_id = 0        │
                                  │   margin_mode != Cross   │
                                  │   或 imf != 0            │
                                  └──────────┬──────────────┘
        │                                    │
        │  SetPositionLeverage               │  SetPositionLeverage
        │  (写非默认杠杆)                     │  (默认值 -> 删行 / 不写)
        │                                    │
        │  OpenPosition                      │  OpenPosition (mut 读到 leverage
        │  (Opened, new id)                  │   字段并 stamp 到新 row 上)
        ▼                                    ▼
    ┌─────────────────────────┐           ┌─────────────────────────┐
    │   Open                  │           │   Open                  │
    │   base_size != 0        │  Mutate   │   base_size != 0        │
    │   position_id = N       │ ────────▶ │   position_id = N       │
    │                         │  Updated  │   (同向 size 变 / funding /
    │                         │           │    isolated margin 调整) │
    └────────┬────────┬───────┘           └─────────────────────────┘
             │        │
             │ Close  │  Flip = ClosePosition + OpenPosition
             │        │  (Closed old id + Opened new id)
             ▼        ▼
   ┌──────────────────────────────┐
   │   row 被 Remove（deleted=true）│
   │   或保留为 leverage-only       │
   │   （deleted=false）            │
   └──────────────────────────────┘
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

- **`UpdatePosition` 不再存在**。所有外部 caller（`x/trade` /
  `x/funding` / `x/account` msg_server）已全量迁移到
  `OpenPosition` / `MutatePosition` / `ClosePosition` / `SetPositionLeverage`。
  下游 module 若新增 caller，请直接走显式方法；任何"写入意图不确定"的逻辑
  bug 都会以 `ErrPositionLifecycleViolation` 早失败而非静默走完一个错误分支。
- `EventPositionUpdated` 仍然存在，但语义收窄到"`MutatePosition` 对已开仓行的
  就地更新 + `SetPositionLeverage` 杠杆配置写入"。升级前依赖该事件区分开仓 /
  平仓的 indexer 实现需要切换到新事件。
- 新增的 `position_id` 字段不会出现在升级前的 fills / 仓位事件里；indexer 建议
  以 `position_id != 0` 作为 "采用新协议" 的判定。
- Genesis 字段 `Counters.next_position_index` 是新增的，升级前的快照默认值 0，
  由 `InitGenesis` 自动 coerce 到 `max(persisted position_id) + 1`，所以无需迁
  移脚本。
- `AccountPosition` 行的物理回收对 `IterateAccountPositions` 的调用方透明：仍
  然只会迭代已存在的行，调用方依然应当用 `pos.BaseSize.IsZero()` 跳过 leverage-
  only 配置行（与升级前同语义）。
- `SettlePositionFunding` 不再在空仓位上 snapshot prefix-sum。代替它，
  `x/trade.applyPositionChange` 的 open 分支在调用 `OpenPosition` 时通过 mut
  闭包从 `MarketDetails.FundingRatePrefixSum` 直接 seed
  `LastFundingRatePrefixSum`，保证开仓后首次 funding settlement 只对开仓后累积
  的部分计费。Test fixture 直接写 `ak.positions[key]` 的也要自行 seed
  prefix-sum（参考 `x/funding/tests/settle_position_test.go`）。
