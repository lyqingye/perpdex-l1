# PerpDEX L1 业务模块 Code Review 报告

审查日期：2026-05-03  
审查范围：`x/asset`、`x/account`、`x/market`、`x/oracle`、`x/orderbook`、`x/matching`、`x/trade`、`x/funding`、`x/risk`、`x/liquidation`，以及相关 app wiring 和 e2e 测试。

## 1. 总体结论

当前代码不建议直接合入或上线到真实资金环境。项目主体结构清晰，Cosmos SDK 模块划分也基本符合工程习惯，但撮合、交易、风险、清算这几条资金核心链路仍存在会导致仓位翻转、风控绕过、资产负数、OI 失真和价格缺失被静默忽略的问题。最主要风险是：maker 侧没有做充分风控、清算数量没有封顶、风险计算对缺失 oracle 价格选择跳过仓位，以及 spot/perp 状态更新没有统一资产守恒校验。已有 `go test ./...` 可以通过，但测试更偏正常路径和 e2e happy path，缺少恶意输入、边界值和状态不一致回归用例。风险等级：Blocker。

## 2. 关键逻辑审查

### 问题 1：perp 撮合只校验 taker 风险，maker 可以被动开出不健康仓位

**严重程度：** Blocker  
**位置：** `x/trade/keeper/keeper.go` `ApplyPerpsMatching` 第 126-136 行；`x/matching/keeper/msg_server.go` `CreateOrder` 第 114-129 行  
**问题说明：** `ApplyPerpsMatching` 只在最后调用 `riskKeeper.IsValidRiskChange` 校验 taker，maker 的仓位、手续费和已实现 PnL 更新后没有做风控检查。maker 的 GTT 挂单在入簿时也没有锁定保证金或预校验可成交后的风险，导致低抵押账户可以先挂单，之后被正常 taker 撞单成交。  
**可能后果：** maker 可在无足够抵押的情况下被动开仓，形成不健康或负抵押仓位；后续亏损由保险基金、ADL 或其他 LP 承担，存在直接资金风险。  
**修改建议：** 对每笔 perp fill 同时校验 maker 和 taker。对于会增加 maker 风险的挂单，入簿时应预估最坏成交风险或至少在成交后拒绝整个 tx；如果依赖 tx cache 回滚，也必须确保 `ApplyPerpsMatching` 返回错误前没有被 EndBlock/内部调用绕过。  
**建议代码：**

```go
if !f.NoRiskCheck {
    for _, accountIndex := range []uint64{f.MakerAccountIndex, f.TakerAccountIndex} {
        ok, err := k.riskKeeper.IsValidRiskChange(ctx, accountIndex)
        if err != nil {
            return err
        }
        if !ok {
            return fmt.Errorf("trade: account %d risk regression", accountIndex)
        }
    }
}
```

### 问题 2：`MsgLiquidate` 未限制清算数量，攻击者可把 victim 直接翻成反向仓位

**严重程度：** Blocker  
**位置：** `x/liquidation/keeper/keeper.go` `Liquidate` 第 72-107 行  
**问题说明：** `Liquidate` 只检查 victim 有非零仓位，没有要求 `baseAmount <= abs(victim.position)`。`applyPositionChange` 支持 flip 场景，因此传入超大 `baseAmount` 会先关闭 victim 原仓，再给 victim 开出反向仓位。`Deleverage` 第 168-173 行有这个检查，但 `Liquidate` 没有。  
**可能后果：** 清算者可以通过一次清算把被清算人从多头变成空头或反过来，制造错误仓位、错误 PnL、错误保险基金垫付，并绕过正常下单/风控路径。  
**修改建议：** 在 `Liquidate` 中与 `Deleverage` 一样检查 `baseAmount` 不超过 victim 当前仓位绝对值；必要时按 health status 计算最大可清算比例。  
**建议代码：**

```go
absVictim := pos.Position.Abs()
if math.NewIntFromUint64(baseAmount).GT(absVictim) {
    return types.ErrInvalidParams.Wrapf(
        "base_amount=%d exceeds victim position size %s",
        baseAmount, absVictim.String(),
    )
}
```

### 问题 3：风险计算在 oracle/market 缺失时静默跳过仓位

**严重程度：** Blocker  
**位置：** `x/risk/keeper/keeper.go` `ComputeRiskInfo` 第 90-105 行  
**问题说明：** 风险模块遍历账户仓位时，如果 `GetPrice` 或 `GetMarketDetails` 返回错误，会 `continue`，即该仓位不计入 TAV、IM、MM 和 close-out requirement。缺失价格、市场详情损坏或 oracle 被删除时，持仓账户会被错误视为更健康。  
**可能后果：** 用户可在价格缺失或 stale price 场景下绕过清算、提现风控或 LP burn 限制；坏账无法被及时发现，资金风险高。  
**修改建议：** 对非零仓位，缺失 price/market details 必须返回错误或按最保守价格计算风险；同时引入 oracle `MaxAgeMs` 检查，过期价格不能用于风控。  
**建议代码：**

```go
px, err := k.oracleKeeper.GetPrice(ctx, marketIdx)
if err != nil {
    return types.RiskInfo{}, fmt.Errorf("risk: missing oracle price for market %d: %w", marketIdx, err)
}
if px.MarkPrice == 0 {
    return types.RiskInfo{}, fmt.Errorf("risk: zero mark price for market %d", marketIdx)
}
```

### 问题 4：Open Interest 被实现成累计成交量，关闭仓位时不会下降

**严重程度：** High  
**位置：** `x/trade/keeper/keeper.go` `ApplyPerpsMatching` 第 122 行；`x/market/keeper/keeper.go` `UpdateOpenInterest` 第 127-141 行；`tests/e2e/e2e_trading_flow_test.go` 第 146-151 行  
**问题说明：** 每笔 perp fill 都以正数 `baseAmount` 调用 `UpdateOpenInterest`，即使双方都在平仓，OI 也继续增加。e2e 测试甚至断言 OI 是累计成交量，但字段名和 `OpenInterestLimit` 语义应限制当前未平仓量。  
**可能后果：** OI limit 会随着历史成交不可逆增长，导致市场被错误限流；风控、资金费率、市场监控如果使用 OI，会得到错误信号。  
**修改建议：** `applyPositionChange` 返回每个账户的 open interest delta，按 `sum(abs(new))-sum(abs(old)) / 2` 或按市场净敞口定义更新；测试应断言 round-trip 后 OI 回到 0。  

### 问题 5：reduce-only、最小下单量、quote limit、order enum 等核心订单约束缺失

**严重程度：** High  
**位置：** `x/matching/types/msgs.go` `MsgCreateOrder.ValidateBasic` 第 23-31 行；`x/matching/keeper/match.go` 第 16-28 行、第 67-72 行；`x/matching/keeper/msg_server.go` 第 19-141 行  
**问题说明：** `ValidateBasic` 只校验 sender 和 `BaseAmount > 0`。实际下单未校验 `OrderType`、`TimeInForce`、`ClientOrderIndex` 范围、limit price、market min base/min quote、`OrderQuoteLimit`、GTT expiry、trigger price，也没有执行注释中的 taker/maker reduce-only invariant。  
**可能后果：** reduce-only 订单可以扩大仓位；非法 enum 可落入普通 limit 分支；低于市场最小量或超 quote limit 的订单可入簿/成交；触发订单和 TWAP 类型可能以错误路径处理。  
**修改建议：** 在 `ValidateBasic` 和 keeper 层同时做 enum/range 校验；在撮合前检查 reduce-only 是否只会降低当前仓位绝对值；用 market 配置校验 base/quote。  

### 问题 6：触发订单没有进入 trigger index，也没有 matching EndBlocker 处理

**严重程度：** High  
**位置：** `x/matching/keeper/msg_server.go` `CreateOrder` 第 86-97 行；`x/orderbook/keeper/orderbook.go` 第 304-312 行；`x/matching/module.go` 第 23-27 行  
**问题说明：** stop-loss/take-profit 订单被标记为 `OrderStatusTriggeredPending` 后只调用 `SetOrder`，没有调用 `bookKeeper.AddTrigger`。同时 `x/matching` module 没有实现 `HasEndBlocker`，虽然 app 的 EndBlocker 顺序里写了 matching trigger scan。  
**可能后果：** 用户提交的触发订单永远不会触发，状态长期挂起；风控订单失效会导致无法止损/止盈。  
**修改建议：** 创建触发订单时写入 `TriggerIndex`；实现 matching EndBlocker，根据 mark/index price 扫描触发条件并转成 IOC/GTT 子订单；取消和修改时同步删除 trigger index。  

### 问题 7：`CancelAllOrders` 是空实现，返回成功但不取消任何订单

**严重程度：** High  
**位置：** `x/matching/keeper/msg_server.go` `CancelAllOrders` 第 167-183 行  
**问题说明：** 函数校验权限和 mode 后直接返回成功，没有遍历用户订单，也没有设置 scheduled cancel/abort 状态。注释写着 MVP，但这是对外 Msg。  
**可能后果：** 用户或风控 bot 以为已撤单，实际订单仍在簿上继续成交，可能造成额外亏损或越权风险。  
**修改建议：** 立即实现用户订单索引遍历和 cap；如果 scheduled 模式暂不支持，应明确返回 `ErrUnimplemented`，不能返回成功。  

### 问题 8：spot 撮合可写出负资产余额

**严重程度：** High  
**位置：** `x/trade/keeper/keeper.go` `ApplySpotMatching` 第 198-227 行；`transferAsset` 第 230-244 行  
**问题说明：** `transferAsset` 直接 `src.Balance = src.Balance.Sub(amount)`，没有像 `AddAccountAssetBalance` 一样检查负数，也没有 spot 可用余额/锁定余额校验。  
**可能后果：** spot 买卖可让账户 base 或 quote 余额变成负数，破坏资产守恒和提现安全。  
**修改建议：** 复用 `accountKeeper.AddAccountAssetBalance` 或在 `transferAsset` 中显式检查 `src.Balance >= amount`；成交前对 maker/taker 资产做可用余额校验和锁定。  

### 问题 9：orderbook 聚合金额存在 uint64 溢出和 int64 截断

**严重程度：** High  
**位置：** `x/orderbook/keeper/orderbook.go` `InsertOrderbookEntry` 第 59 行；`PartialFill` 第 107-121 行；`uint64Mul` 第 205 行  
**问题说明：** `RemainingBaseAmount * Price` 用 `uint64` 直接相乘，再转成 `int64` 作为 signed delta。协议常量允许 `baseAmount` 到 `281_474_976_710_655`、price 到 `4_294_967_295`，乘积远超 `uint64`。  
**可能后果：** price level quote 聚合被溢出污染，影响 depth、impact price、funding premium 和行情查询；恶意大单可操纵 funding 或让聚合值归零/反向。  
**修改建议：** 使用 `math.Int` 或 `big.Int` 存储 quote aggregate；下单时强制 `base * price <= MaxOrderQuoteAmount` 并使用 checked multiplication。  

### 问题 10：client order index 可以重复，取消旧单会误删新单索引

**严重程度：** Medium  
**位置：** `x/orderbook/keeper/orderbook.go` `IndexClientOrder` 第 294-301 行；`x/matching/keeper/msg_server.go` 第 131-134 行、第 156-164 行  
**问题说明：** `(market, account, client_order_index)` 写入前没有检查是否已存在。重复 client id 会覆盖旧映射；之后取消旧订单时 `UnindexClientOrder` 会删除同一个 key，使新订单无法通过 client id 查询。  
**可能后果：** 客户端幂等语义失效，订单查询/撤单错乱；在高频交易中容易造成重复下单或无法撤单。  
**修改建议：** 创建订单时如果 client id 已映射到 open/pending order，应返回重复错误或执行幂等返回；取消时只在映射仍指向当前 orderIndex 时删除。  

### 问题 11：取消/修改订单没有校验订单状态

**严重程度：** Medium  
**位置：** `x/matching/keeper/msg_server.go` `CancelOrder` 第 143-164 行；`ModifyOrder` 第 186-225 行  
**问题说明：** `RemoveOrderbookEntry` 找不到 entry 时返回 nil，因此已成交、已取消或 trigger pending 订单也可以被 `CancelOrder` 改写成 cancelled。`ModifyOrder` 也没有限制只能修改 open/resting 订单，且忽略了部分 `SetOrder`/unindex 错误。  
**可能后果：** 历史成交订单状态被覆盖，查询和审计不可信；trigger pending 订单可能绕过 trigger index 清理；客户端状态机出现分叉。  
**修改建议：** 只允许 `Status == Open && RemainingBaseAmount > 0` 的 resting order 被取消/修改；trigger pending 走专门路径；所有 store 写错误必须返回。  

### 问题 12：oracle 价格没有 staleness 校验，PoS vote extension 路径未真正注入聚合结果

**严重程度：** Medium  
**位置：** `x/oracle/keeper/keeper.go` `GetPrice` 第 61-70 行；`x/oracle/types/params.go` 第 5-18 行；`x/oracle/keeper/voteext.go` 第 24-75 行  
**问题说明：** `Params.MaxAgeMs` 存在但 `GetPrice`、risk、funding 都没有检查价格年龄。vote extension 打开时 `ExtendVote` 发送空 payload，`PrepareProposal`/`ProcessProposal` 只是调用 default handler，未注入 `MsgAggregateOracleVotes`。  
**可能后果：** 风控和清算可能长期使用过期价格；切到 PoS median 模式后没有真实价格流，系统依赖手工 authority 聚合，活性和安全边界不清晰。  
**修改建议：** 在 oracle keeper 暴露 `GetFreshPrice(ctx, marketIdx)`；risk/funding/liquidation 统一使用 fresh price；PoS 模式未完成前应保持不可启用或显式返回错误。  

### 问题 13：market 和 leverage 更新缺少业务参数校验

**严重程度：** Medium  
**位置：** `x/market/keeper/msg_server.go` `UpdateMarket` 第 87-105 行；`UpdateMarketDetails` 第 108-132 行；`x/account/keeper/msg_server.go` `UpdateLeverage` 第 324-345 行  
**问题说明：** `UpdateMarket` 没有调用 `ValidateBasic`，也不校验 status enum、fee 是否小于 `FeeTick`、min base/quote 是否大于 0、expiry 是否合理。`UpdateLeverage` 不校验 margin mode enum、IMF 与市场 min/default 的关系，也不校验 market 是否存在。  
**可能后果：** governance 或测试 helper 可把市场配置改成不可成交/不可清算的状态；非法 margin mode 会落到 cross 分支或造成未来维护误判。  
**修改建议：** 为每个 Msg handler 开头统一调用 `ValidateBasic`；增加 market config validator；`UpdateLeverage` 读取 market details 并校验 `new_initial_margin_fraction >= MinInitialMarginFraction`、mode 属于 cross/isolated。  

### 问题 14：funding 采样/结算错误被吞掉，且 depth 消失时 impact price 会沿用旧值

**严重程度：** Medium  
**位置：** `x/funding/keeper/abci.go` `BeginBlocker` 第 28-33 行；`SettleAllMarkets` 第 84-91 行；`processMarketSample` 第 57-75 行  
**问题说明：** BeginBlocker 中 `processMarketSample` 和 `settleMarket` 的错误都被 `_ =` 忽略。`processMarketSample` 只有 bid/ask 都大于 0 时才更新 `ImpactPrice`，否则不清零，后续在 oracle index 存在时仍可能用旧 impact price 累加 premium。  
**可能后果：** 某个市场 funding 失败时链不会暴露错误；流动性消失后继续按旧 depth 计算 funding premium，资金费率被错误累积。  
**修改建议：** 返回并记录 per-market error，至少 emit event；bid/ask 缺任一侧时清空 impact price 或停止采样；落实 `MaxPremiumSampleCount`。  

### 问题 15：genesis 校验和接口语义存在不一致

**严重程度：** Low  
**位置：** `x/asset/types/genesis.go` 第 28-44 行；`x/account/types/genesis.go` 第 38-49 行；`proto/perpdex/account/v1/tx.proto` 第 141-146 行；`x/account/keeper/msg_server_publicpool.go` 第 34-44 行  
**问题说明：** asset/account genesis 只检查重复 index/denom，不校验 asset 参数、account 类型、counter 范围、PublicPoolInfo 和保险基金账户不变量。proto 注释说 `MsgCreatePublicPool` 可创建 PUBLIC_POOL 或 INSURANCE_FUND，但 keeper 实现只允许 PUBLIC_POOL。  
**可能后果：** 非默认 genesis 或迁移状态可带入无效资产/账户；文档、客户端和 keeper 行为不一致，后续集成容易误用。  
**修改建议：** 在 genesis `Validate` 复用 Msg/Params 的完整校验；同步 proto 注释和 keeper 语义，或恢复明确的 IF 创建路径。  

## 3. 边界条件和异常路径

| 场景 | 当前代码行为 | 风险 | 建议 |
|---|---|---|---|
| 非零仓位但 oracle price 缺失 | `ComputeRiskInfo` 直接跳过该仓位 | 账户被错误评为健康，可提现/逃避清算 | 缺价返回错误或使用保守价；检查 `MaxAgeMs` |
| oracle mark/index price 为 0 | 多处允许写入；部分查询返回 0，风险计算可能异常 | 风控、funding、清算价格失真 | `InjectOracle`/聚合结果拒绝 0 价格 |
| liquidation `baseAmount > abs(position)` | `ApplyPerpsMatching` flip victim 仓位 | 清算变开仓，状态严重错误 | `Liquidate` 增加数量上限 |
| maker 无抵押挂单后被成交 | 只校验 taker 风险 | maker 可形成坏账 | 成交后 maker/taker 都校验；入簿时预校验 |
| reduce-only 订单扩大仓位 | 未实现 reduce-only invariant | 用户以为只减仓，实际增仓 | 下单和每次 fill 前检查仓位方向与绝对值变化 |
| GTT/trigger order 到期或触发 | GTT 只清理；trigger 不入 trigger index | 止损/止盈永远不触发 | 实现 trigger index 和 matching EndBlocker |
| CancelAllOrders | 返回成功但不撤单 | 用户误判风险已解除 | 实现或返回 unsupported 错误 |
| 重复 `client_order_index` | 新映射覆盖旧映射 | 查询/撤单错乱 | 创建时做唯一性或幂等校验 |
| 取消已成交订单 | 找不到 book entry 仍返回 nil 并改成 cancelled | 历史状态被污染 | 校验 order status/open amount |
| 超大 base * price | uint64 溢出后再 int64 转换 | depth/funding 被操纵 | checked multiplication，使用大整数 |
| spot 余额不足成交 | `transferAsset` 写负余额 | 资产不守恒 | 负数检查、余额锁定 |
| 市场平仓 round-trip | OI 增加到 2 倍成交量 | OI limit 失真 | 用当前未平仓量更新 OI |
| funding 单市场错误 | BeginBlocker 吞错 | 市场 funding 静默停摆 | 返回错误或显式事件告警 |
| book 单边无深度 | `ImpactPrice` 沿用旧值 | funding premium 错算 | 单边缺失时清零或跳过样本 |
| 非法 enum：order type/TIF/margin mode/status | 多处未校验 | 落入错误默认分支 | Msg + keeper 双层 enum 校验 |
| `MsgDeleverage` sender 与 deleverager 无绑定 | handler 不校验 sender 是否有权使用 deleverager | 任意人可发起强制 ADL 选择第三方 | 明确 keeper bot 权限或按链上 ADL 队列自动执行 |

## 4. 安全性审查

权限方面，governance authority 的基本比较存在，但部分 handler 没有统一调用 `ValidateBasic`，且 `MsgDeleverage` 没有校验 sender 与 `DeleveragerAccountIndex` 的关系。如果手动 ADL 是公开 keeper bot 行为，需要把可选 counterparty 的规则写进链上队列；否则当前接口允许任意人指定第三方账户参与强制 deleverage。

资金和仓位守恒方面，perp 成交只检查 taker 风险，spot 成交允许负资产余额，liquidation 数量不封顶，OI 不是当前未平仓量。这些不是风格问题，而是直接影响 collateral、position、PnL 和保险基金承担范围的安全问题。

oracle 安全边界不够清晰。风险模块过度信任 `GetPrice` 成功路径，又在失败时静默忽略仓位；同时没有价格新鲜度检查。PoS median 的 vote extension 注释与实现差距较大，切换模式前应视为未完成能力。

重放/重复提交方面，client order id 缺少幂等保护，重复 id 会覆盖索引；Cancel/Modify 对订单状态没有防御，会污染历史状态。并发层面 Cosmos 单线程执行降低了链上重入风险，但 EndBlocker/内部 keeper 调用使用同一套写路径时，`NoRiskCheck` 和错误吞掉会放大状态不一致。

## 5. 代码规范和可维护性

- 注释和实现多处不一致：`ApplyPerpsMatching` 注释写 maker 也会校验，实际只校验 taker；`CancelAllOrders` 对外成功但实现为空；`MsgCreatePublicPool` proto 注释允许 IF，keeper 拒绝 IF。建议把未实现功能从成功路径移除，改成显式错误。
- 风控逻辑分散在 account/trade/liquidation/risk 多处，且 `NoRiskCheck` 语义过宽。建议把“普通成交、清算、市场过期、IF 吸收、用户 ADL”的风险例外做成枚举原因，并集中审计。
- `Risk.Cache` 设计为“pre-state risk cache”，但项目内没有调用 `SnapshotPreRisk`，也没有清理逻辑。当前 `IsValidRiskChange` 实际只允许 post healthy，不支持注释中的“严格改善”。要么实现 ante/msg 前置快照和 tx 后清理，要么删除 cache 语义避免误导。
- 大量 Msg `ValidateBasic` 只校验地址，不校验 enum/range/业务上下限。建议每个模块建立 `ValidateMsgXxx(ctx, msg)`，把需要 keeper 状态的校验放 keeper 层。
- generated pb 文件较多，业务审查应固定只看非生成文件；建议增加 module-level unit tests，而不是只依赖 e2e helper。
- 查询层很多方法没有 nil request 检查，虽然不是资金路径，但 public gRPC 面更稳妥的做法是统一返回 `InvalidArgument`。

## 6. 测试建议

| 优先级 | 测试场景 | 为什么需要 |
|---|---|---|
| P0 | maker 无抵押挂 GTT ask，被健康 taker 撞单，应拒绝或回滚 | 覆盖 maker 风控缺失的资金风险 |
| P0 | `MsgLiquidate` 传入大于 victim 仓位的 `baseAmount`，断言不能 flip | 防止清算变反向开仓 |
| P0 | 非零仓位删除/缺失 oracle price 后，提现和健康查询必须失败 | 防止风险计算跳过仓位 |
| P0 | spot 成交时 maker/taker base/quote 不足，断言不会出现负余额 | 资产守恒核心测试 |
| P0 | 平仓 round-trip 后 `OpenInterest == 0` | 修复 OI 当前量语义 |
| P1 | reduce-only long sell/long buy、short buy/short sell 四种方向测试 | 覆盖符号和减仓边界 |
| P1 | duplicate client order id：第二次提交应幂等或失败；取消旧单不影响新单索引 | 防止订单索引错乱 |
| P1 | Cancel filled/cancelled/trigger-pending order 应失败且不改历史状态 | 保证订单状态机一致 |
| P1 | 超大 `baseAmount * price` 下单应被 quote limit 拒绝 | 防止 orderbook 聚合溢出 |
| P1 | trigger order 写入 trigger index，价格穿越后触发成交/入簿 | 覆盖止损/止盈路径 |
| P1 | CancelAllOrders immediate/scheduled/abort 三种模式 | 防止空实现继续返回成功 |
| P1 | oracle stale price 超过 `MaxAgeMs` 时 risk/funding/liquidation 拒绝使用 | 覆盖时间边界 |
| P1 | funding 单边 book 缺失时不累加旧 impact premium | 防止 funding 被旧深度污染 |
| P2 | `UpdateMarket` 非法 status/fee/min amount/expiry 测试 | governance 参数防御 |
| P2 | `UpdateLeverage` 非法 margin mode 和低于市场 min IMF 测试 | 防止非法配置落入默认分支 |
| P2 | genesis 中重复/非法 asset、account、pool info 校验 | 防止迁移或自定义 genesis 带入坏状态 |

## 7. 推荐修改摘要

- [ ] 必须修改的问题：maker 风控缺失、`Liquidate` 数量未封顶、risk 缺价跳过仓位、spot 负余额、OI 错误语义、orderbook 溢出。
- [ ] 建议修改的问题：reduce-only/order enum/market limit 校验、trigger order 和 CancelAllOrders 实现、client order id 幂等、Cancel/Modify 状态机校验。
- [ ] 建议补充的测试：P0/P1 表格中的资金、仓位、oracle、订单状态和 rounding/符号测试。
- [ ] 可以后续优化的问题：risk cache 设计、genesis 校验增强、PoS oracle vote extension 完整实现、查询 nil request 统一处理、proto 注释与 keeper 行为同步。

验证：本次审查期间运行 `go test ./...`，所有现有测试通过；结论中的问题主要来自静态业务逻辑审查和现有测试未覆盖的对抗路径。
