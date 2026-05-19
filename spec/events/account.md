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
`EventPositionUpdated`。这种"一招吃遍天"的 API 形态既让 caller 的意图变得不显
式，也让 indexer 必须对比新旧 `base_size` 才能判断仓位边界。

第一轮重构曾尝试把它拆成 `OpenPosition` / `MutatePosition` / `ClosePosition` 三个
显式方法，但仍然以 `func(*Position) error` mutator 闭包的形式暴露给外部 caller —
这种 API 形态与原来的 `UpdatePosition` 没有本质区别：外部 caller 仍然在直接操控
仓位的字段，`x/account` 实际上只承担了一个"持久化 + 发事件"的薄壳。

**最终设计** 把 `applyPositionChange` 的全部分支判断（open / mutate / close / flip）
都搬到 `x/account.Keeper.ApplyFill` 里：

1. **API 边界清晰**：外部 caller 只调"应用一笔 fill"，不知道、也不需要知道仓位
   生命周期到底走了哪条分支。"写一个 closure 改字段"的口子彻底关闭。
2. **内聚性**：仓位字段的所有变更逻辑（fill apply、funding settle、isolated margin
   调仓、leverage 配置、flip 编排）都内聚在 `x/account` 内部；package-private 的
   `openPosition` / `mutatePosition` / `closePosition` 只是 `ApplyFill` 的实现细
   节，对外部不可见。
3. **唯一 `position_id`**：每段仓位生命周期都分配一个全局唯一 id，可以作为 fills
   / funding / 调仓事件的 join key。
4. **完全平仓后存储回收**：`AccountPosition` 行在默认杠杆时直接从 KV 移除，与
   "被取消的订单也应该从 store 移除"的理念一致。

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

完全平仓时（`base_size: !=0 → 0`），`ApplyFill` / `ClosePosition` 内部按"杠杆是
否为默认"决定是否回收 KV 行：

- **默认杠杆**（`margin_mode == Cross && initial_margin_fraction == 0`）：直接
  `AccountPositions.Remove(...)`，释放 KV 空间，事件 `deleted = true`。
- **非默认杠杆**：保留行但把 `base_size` / `entry_quote` /
  `last_funding_rate_prefix_sum` / `allocated_margin` / `position_id` 全部重置
  为零，仅保留 `margin_mode` / `initial_margin_fraction`，事件 `deleted = false`。

这样用户配置过的杠杆偏好能跨"平仓 → 再开仓"周期存活，同时绝大多数交叉默认杠
杆的用户依然享受存储自动回收。

---

## Keeper API

`x/account.Keeper` 暴露 **若干语义内聚的公开方法**，每个方法只负责一种业务级
任务，并强制相应的前后置不变量；任何违反不变量的 caller 都会以
`ErrPositionLifecycleViolation` 早失败。**外部 caller 不再需要传 `func(*Position)
error` 闭包** —— 所有字段级写入都封装在 `x/account` 内部：

| 方法 | 用途 | 事件 |
|------|------|------|
| `ApplyFill(acc, mkt, price, baseAmount, sign, fundingRatePrefixSum)` | x/trade 的撮合主入口：根据 fill 计算 `pre.ApplyFill(delta, price)`，按 transition 自动 dispatch open / mutate / close / flip，并完成所有 KV 写入 | `EventPositionOpened` / `EventPositionUpdated` / `EventPositionClosed`（flip 时同笔 tx 内先 Closed 再 Opened） |
| `AdjustAllocatedMargin(acc, mkt, delta)` | 调整 isolated margin pool 的 `AllocatedMargin += delta`，用于 isolated 三步对账（PnL/fee 折入、liqFee 抵扣、position requirement rebalance）以及 `MsgUpdateMargin` | `EventPositionUpdated`（要求 `pre.BaseSize != 0`） |
| `ApplyFundingPayment(acc, mkt, newPrefixSum)` | 把累积 funding 折入 `EntryQuote` 并 snapshot 新的 prefix sum；空仓位 / 零 delta no-op | `EventPositionUpdated`（仅当真的写了） |
| `SetPositionLeverage(acc, mkt, mode, imf)` | 写一个 `BaseSize == 0, position_id == 0` 的 leverage-only 配置行 | `EventPositionUpdated`（`position_id == 0`） |
| `ClosePosition(acc, mkt)` | 强制平仓入口（清算 / market expiry / IF / ADL 接管），返回 pre-close 快照供 caller 走下游对账 | `EventPositionClosed` |

包私有（外部不可调用）的三个生命周期 primitive：

| primitive | 责任 |
|------|------|
| `openPosition(post)` | 分配 `position_id`、stamp `CreatedAt`、`setPosition`、emit `EventPositionOpened` |
| `mutatePosition(pre, post)` | 保留 `position_id`、`setPosition`、emit `EventPositionUpdated` |
| `closePosition(pre)` | 按杠杆决定 remove 还是保留为 leverage-only、emit `EventPositionClosed`、返回 pre-close 快照 |

`ApplyFill` 即在内部按 transition 调度这三个 primitive；外部 caller **无法**
直接绕过 `ApplyFill` 写仓位字段。

### Flip 由 `ApplyFill` 内部完成

若 fill 越过零点（`fill.SideFlipped == true`），`ApplyFill` 在同一笔 tx 内：

1. 调 `closePosition` —— 关闭旧仓位（emit `EventPositionClosed`，旧 `position_id`）。
2. 把 `AllocatedMargin` / `LastFundingRatePrefixSum` 搬到 residual leg 上。
3. 调 `openPosition` —— 用残量开新仓位（emit `EventPositionOpened`，新
   `position_id`）。

Indexer 看到的序列是严格有序的 `EventPositionClosed`（旧 id）→
`EventPositionOpened`（新 id），不需要自己根据 fill 数学判断。

### Caller 责任分配

| 场景 | 调用方 → x/account |
|------|----------|
| 普通开仓 / 同向加减仓 / 完全平仓 / flip | `x/trade.Engine.Apply` → `ApplyFill` |
| 资金费率结算 | `x/funding.Keeper.SettlePositionFunding` → `ApplyFundingPayment` |
| Isolated margin 调仓（PnL/fee 折入、liqFee 抵扣、position_requirement rebalance） | `x/trade.Engine.applyIsolatedAccount` / `rebalanceIsolatedMargin` → `AdjustAllocatedMargin` |
| `MsgUpdateMargin` 加减保证金 | `x/account.msgServer.UpdateMargin` → `AdjustAllocatedMargin` |
| `MsgUpdateLeverage` 调杠杆 | `x/account.msgServer.UpdateLeverage` → `SetPositionLeverage`（仅当 `BaseSize == 0`） |
| Genesis 恢复 | `setPosition`（package-private，跳过事件与 ID 分配） |

### `fundingRatePrefixSum` 为什么作为参数传入

`ApplyFill` 的 open / flip 分支需要把市场当前的 `FundingRatePrefixSum` 写入
`LastFundingRatePrefixSum`（开仓边界），但 `x/account` 本模块**不**直接持有
`marketKeeper`：Cosmos 的 late-bound keeper 在 `x/trade` 持有的 `accountKeeper`
interface 副本上不可见。所以由 `x/trade` 在调用 `ApplyFill` 之前先
`marketKeeper.GetMarketDetails` 拿到 prefix sum，然后作为参数传入；
`x/account` 仅在 open / flip 时真正使用，pure-close / same-side 分支会忽略。

---

## 事件

### EventPositionOpened

由 `ApplyFill` 在 open 与 flip-residual 两种 transition 内调 `openPosition` 时发
射，携带 **刚分配的 `position_id`** 与开仓后的完整快照。

```proto
message EventPositionOpened {
  AccountPosition position = 1;
}
```

**触发场景**

- 普通开仓：用户通过 `MsgCreateOrder` 撮合产生 fill，`pre.BaseSize == 0`
  被 `ApplyFill` 路由到 open 分支。
- Flip 的 open 半幕：`fill.SideFlipped == true`，`ApplyFill` 在同一笔 tx 内先发
  `EventPositionClosed`、再发 `EventPositionOpened`。
- Liquidation / ADL 接管方（LLP / IF / 高杠杆账户）首次承接仓位 —— 仍走撮合
  路径，最终落到 `ApplyFill` 的 open 分支。

**Invariants**

- `position.position_id != 0`，且全网历史 ID 唯一。
- `position.base_size != 0`。
- `position.created_at` 等于该事件所在块的 `BlockTime` 毫秒。
- 紧邻这条事件的 `EventOrderFilled` 中相应一侧的 `*_position_id` 等于
  `position.position_id`（taker 与 maker 各自独立判断）。

---

### EventPositionUpdated

由多个 cohesive 方法发射，覆盖 "已开仓行的字段级更新"：

- `ApplyFill` 的 **same-side change** 分支（同向加仓 / 同向减仓但未归零）。
- `AdjustAllocatedMargin`（isolated 三步对账 / `MsgUpdateMargin`）。
- `ApplyFundingPayment`（funding 折入；空仓位 / 零 delta 不发事件）。
- `SetPositionLeverage`（leverage-only 配置写入，携带
  `position_id == 0` 以区别于真实仓位更新）。

```proto
message EventPositionUpdated {
  AccountPosition position = 1;
}
```

**Invariants**

- 真实仓位更新（前 4 类来源的前 3 项）：`position.position_id` 等于上一次该
  `(account, market)` 的 `EventPositionOpened.position.position_id`，
  `position.base_size` 与上一次同号且不为零。
- 杠杆配置写入（`SetPositionLeverage`）：`position.position_id == 0` 且
  `position.base_size == 0`，至少一个 `margin_mode` /
  `initial_margin_fraction` 字段非默认。
- side flip **不会** 发 Updated；它走 `closePosition + openPosition` 两条事件。

---

### EventPositionClosed

由 `ApplyFill` 的 pure-close / flip-old-leg 分支以及强制平仓入口
`ClosePosition` 发射。携带 **被关闭的 `position_id`** 与平仓后的快照
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

- 普通完全平仓：`fill.Position.BaseSize == 0`，`ApplyFill` 路由到 close 分支。
- Flip 的 close 半幕：`fill.SideFlipped == true`，`ApplyFill` 先发 Closed 再发
  Opened。
- Market expiry / IF / ADL 强制平仓 —— 通过 `ClosePosition` 入口直接平仓。

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

`Keeper.ClosePosition` 与 `ApplyFill` 的 close 分支返回 `FillApplyResult.New`
都是 **pre-close 快照**（`BaseSize` / `AllocatedMargin` / `EntryQuote` 等仍为
close 之前的值，但事件 payload 已经把 `BaseSize` / `EntryQuote` zeroed），用
途是让 caller 自行处理下游 reconciliation：

- Cross margin：`applyCrossAccount` 把 `realized_pnl - fee` 加到 cross
  collateral 即可，不需要碰 pre-close 字段。
- Isolated margin：`applyIsolatedAccount` 在 close 分支不调
  `AdjustAllocatedMargin`（仓位已不存在），而是把
  `pre.AllocatedMargin + realized_pnl - fee - liq_fee` 一次性 `AddCollateral`
  回 cross collateral，等价于旧版本"add → rebalance → drain"三步串联，但少
  了两次写入和两条 Updated 事件。

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
        │  (写非默认杠杆)                     │  (默认值 -> 不写)
        │                                    │
        │  ApplyFill (open 分支)             │  ApplyFill (open 分支，从既存
        │  (Opened, new id)                  │   leverage 行继承 mode / imf)
        ▼                                    ▼
    ┌─────────────────────────┐           ┌─────────────────────────┐
    │   Open                  │           │   Open                  │
    │   base_size != 0        │ same-side │   base_size != 0        │
    │   position_id = N       │ ────────▶ │   position_id = N       │
    │                         │  Updated  │   (ApplyFill 同向加减 /  │
    │                         │           │    AdjustAllocatedMargin │
    │                         │           │    / ApplyFundingPayment)│
    └────────┬────────┬───────┘           └─────────────────────────┘
             │        │
             │ close  │  Flip (ApplyFill 内部完成：close old + open new)
             │        │  (Closed old id + Opened new id 同笔 tx 内)
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

- **`UpdatePosition` / `OpenPosition` / `MutatePosition` / `ClosePosition`（带
  mut 闭包的版本）已经全部移除**。所有外部 caller（`x/trade` / `x/funding` /
  `x/account` msg_server）已迁移到 cohesive 方法 `ApplyFill` /
  `AdjustAllocatedMargin` / `ApplyFundingPayment` / `SetPositionLeverage` /
  `ClosePosition`。下游模块若新增 caller，请直接走对应的 cohesive 方法；任何
  "写入意图不确定"的逻辑 bug 都会以 `ErrPositionLifecycleViolation` 早失败而
  非静默走完一个错误分支。
- `EventPositionUpdated` 仍然存在，但语义收窄到"已开仓行的字段级更新"。升级
  前依赖该事件区分开仓 / 平仓的 indexer 实现需要切换到三种新事件。
- 新增的 `position_id` 字段不会出现在升级前的 fills / 仓位事件里；indexer 建议
  以 `position_id != 0` 作为 "采用新协议" 的判定。
- Genesis 字段 `Counters.next_position_index` 是新增的，升级前的快照默认值 0，
  由 `InitGenesis` 自动 coerce 到 `max(persisted position_id) + 1`，所以无需迁
  移脚本。
- `AccountPosition` 行的物理回收对 `IterateAccountPositions` 的调用方透明：仍
  然只会迭代已存在的行，调用方依然应当用 `pos.BaseSize.IsZero()` 跳过 leverage-
  only 配置行（与升级前同语义）。
- `SettlePositionFunding` 不再在空仓位上 snapshot prefix-sum；改由 `ApplyFill`
  的 open 分支接收 `fundingRatePrefixSum` 参数直接 seed
  `LastFundingRatePrefixSum`，保证开仓后首次 funding settlement 只对开仓后累积
  的部分计费。
