# account 模块 Code Review 报告

审查日期：2026-05-03  
审查范围：`x/account` 非生成代码、`proto/perpdex/account/v1` 接口定义，以及 account 与 `risk`、`funding`、`bank`、`asset` 的关键交互路径。

## 1. 总体结论

当前 account 模块不建议直接合入到真实资金环境，风险等级：Blocker。模块的基础结构和 Cosmos SDK 写法基本清晰，但资金入口、资金出口、public pool 份额状态、funding 结算和 genesis/export 这些账户核心状态路径存在高风险缺口。最主要风险是 public pool 账户可以被普通 `Withdraw`/`Transfer` 路径绕过份额系统直接转出资金，以及 `Withdraw` 的 perps route 没有复用 `Deposit` 的 margin asset 校验，可能用通用 collateral 提走非保证金币种。已有 `go test ./...` 可以通过，但测试没有覆盖这些对抗路径，说明当前测试偏 happy path，不能作为资金安全信号。

## 2. 关键逻辑审查

### 问题 1：public pool 账户可通过普通账户操作绕过份额系统转出资金

**严重程度：** Blocker  
**位置：** `x/account/keeper/msg_server.go` `Withdraw` 第 96-157 行、`Transfer` 第 221-267 行；`x/account/keeper/account.go` `IsAuthorized` 第 184-192 行；`x/account/keeper/msg_server_publicpool.go` `CreatePublicPool` 第 96-104 行  
**问题说明：** `CreatePublicPool` 创建的 pool account 会把 `OwnerAddress` 设置为 operator 的 master owner。`IsAuthorized` 只判断 `a.OwnerAddress == signer`，因此 operator 对 pool account 调用普通 `MsgWithdraw`、`MsgTransfer`、`MsgUpdateMargin` 等路径会通过权限检查。account 普通路径没有拒绝 `PublicPoolAccountType`，也不检查 `PublicPoolInfo.TotalShares`、LP share entry、cooldown、operator share rate 或 pool status。  
**可能后果：** pool operator 可以直接把 pool collateral 提到银行账户或转回自己的 master/sub account，LP 的 `PublicPoolShare` 和 pool 的 `TotalShares` 不变，NAV 被打到 0 或严重失真。非 operator LP 随后 burn 只能拿到错误金额，属于直接资金风险。  
**修改建议：** 在所有普通用户账户操作入口加 pool/insurance fund 禁止条件，只允许 public pool 资金通过 `MintShares`、`BurnShares`、`StrategyTransfer`、liquidation/ADL 等专用路径变更。低层 `AddCollateral` 可以保留给内部 keeper，但 Msg 层必须 fail closed。  
**建议代码：**

```go
func (m msgServer) rejectPoolAccount(ctx context.Context, idx uint64) error {
	a, err := m.GetAccount(ctx, idx)
	if err != nil {
		return err
	}
	if IsPoolAccount(a) {
		return types.ErrInvalidAccountType.Wrap("pool accounts cannot use generic account msg")
	}
	return nil
}
```

### 问题 2：`Withdraw` 的 perps route 没有校验资产是否可作为保证金

**严重程度：** Blocker  
**位置：** `x/account/keeper/msg_server.go` `Deposit` 第 49-54 行、`Withdraw` 第 120-130 行  
**问题说明：** `Deposit` 对 `RouteTypePerps` 明确要求 `asset.MarginMode == MarginModeEnabled`，但 `Withdraw` 没有对应校验。只要 asset enabled 且 module account 里有该 denom，用户就可以选择非保证金币种并走 perps route，代码会扣减账户通用 `Collateral`，然后从模块账户发送该非保证金币种。  
**可能后果：** 如果其他用户 deposit 了 BTC 等 spot asset 到模块账户，攻击者可用 USDC collateral 通过 `RouteTypePerps` 提走 BTC denom，且不会扣减自己的 BTC spot balance。这破坏资产隔离和模块账户 denom 守恒。  
**修改建议：** `Withdraw` 的 route 校验应与 `Deposit` 对称。`RouteTypePerps` 必须只允许 margin-enabled asset，且最好进一步显式绑定 canonical USDC collateral asset；非 margin asset 只能从 `AccountAsset` spot balance 提现。  
**建议代码：**

```go
case perptypes.RouteTypePerps:
	if asset.MarginMode != perptypes.MarginModeEnabled {
		return nil, types.ErrAssetNotMargin
	}
	if err := m.AddCollateral(ctx, msg.AccountIndex, delta.Neg()); err != nil {
		return nil, err
	}
```

### 问题 3：account 资金变更路径没有结算 pending funding，风控使用的是 stale position

**严重程度：** High  
**位置：** `x/account/keeper/keeper.go` `fundingKeeper` 第 23-25 行、第 74-75 行；`x/account/keeper/msg_server.go` `Withdraw` 第 120-140 行、`Transfer` 第 242-265 行、`UpdateMargin` 第 294-320 行；`x/funding/keeper/keeper.go` `SettlePositionFunding` 第 63-86 行；`x/risk/keeper/keeper.go` `ComputeRiskInfo` 第 98-113 行  
**问题说明：** account keeper 已经 late-bind 了 `fundingKeeper`，但 `Withdraw`、`Transfer`、`UpdateMargin`、`UpdateLeverage` 都没有调用 `SettlePositionFunding`。funding 结算会更新 position 的 `EntryQuote` 和 `LastFundingRatePrefixSum`，而 risk 当前直接用 position 里的 `EntryQuote` 计算 uPnL。未结算时，risk check 看到的是旧 funding 状态。  
**可能后果：** 用户可以在 funding 变差但尚未通过 trade settle 的窗口内提现、转出 collateral 或移除 isolated margin，风控可能错误放行。资金费率亏损没有先落到账户状态，可能把亏损留给后续清算、保险基金或 LP。  
**修改建议：** 在所有会减少可用 collateral 或改变 margin allocation 的 account Msg 前结算相关 position funding。`UpdateMargin`/`UpdateLeverage` 至少结算目标 market；`Withdraw`/`Transfer` 需要结算账户所有非零 perp positions，或者把 pending funding 纳入 risk keeper 的实时计算。`riskKeeper == nil` 时不应跳过风险检查，应返回内部错误。  
**建议代码：**

```go
if err := m.SettleAllPositionFunding(ctx, msg.AccountIndex); err != nil {
	return nil, err
}
```

### 问题 4：operator burn 可把 `operator_shares` 降到 0 并绕过最小 operator share rate

**严重程度：** High  
**位置：** `x/account/keeper/msg_server_publicpool.go` `burnSharesCore` 第 621-635 行；`x/account/keeper/public_pool.go` `CheckMinOperatorShareRate` 第 141-144 行  
**问题说明：** burn 后只在 `!frozen && info.OperatorShares.IsPositive()` 时检查 `CheckMinOperatorShareRate`。如果 operator 一次 burn 到 `operator_shares == 0`，即使 `total_shares > 0` 且 `min_operator_share_rate > 0`，检查也会被跳过。代码注释说“Non-frozen operator burn must keep min_operator_share_rate”，实现却没有覆盖 operator shares 归零场景。  
**可能后果：** operator 可以撤出全部 skin-in-the-game，而非 operator LP 仍持有 pool shares。后续 pool 风险完全由 LP 承担，违反 public pool 的核心经济约束。  
**修改建议：** 对非 frozen pool 始终检查 invariant。`total_shares == 0` 时 `CheckMinOperatorShareRate` 本身会通过，不需要用 `OperatorShares.IsPositive()` 特判。  
**建议代码：**

```go
if !frozen && !CheckMinOperatorShareRate(*info) {
	return nil, types.ErrOperatorRateViolation
}
```

### 问题 5：`ExportGenesis` 丢弃 spot balance、positions 和 account metas

**严重程度：** High  
**位置：** `x/account/keeper/genesis.go` `InitGenesis` 第 18-31 行、`ExportGenesis` 第 42-63 行  
**问题说明：** `InitGenesis` 会导入 `AccountAssets`、`AccountPositions`、`AccountMetas`，但 `ExportGenesis` 只导出 `Params`、`Counters` 和 `Accounts`。这意味着状态导出、迁移或链重启使用 exported genesis 时，spot balances、perp positions、funding snapshots、isolated margin、account metas 会全部丢失。  
**可能后果：** 非 USDC spot 余额消失，perp 仓位消失或风险状态重置，账户资产与 bank module 的 module account 余额不再对应，属于严重状态不一致。  
**修改建议：** 为 `AccountAssets`、`AccountPositions`、`AccountMetas` 实现迭代导出，并增加 export/import round-trip 测试。  
**建议代码：**

```go
assets := []types.AccountAsset{}
iter, err := k.AccountAssets.Iterate(ctx, nil)
// append iter.Value() into assets, positions, metas, then fill GenesisState
```

### 问题 6：spot transfer 可以给不存在的 account 写入 `AccountAsset`

**严重程度：** Medium  
**位置：** `x/account/keeper/msg_server.go` `Transfer` 第 249-255 行；`x/account/keeper/account.go` `GetAccountAsset` 第 145-156 行、`AddAccountAssetBalance` 第 170-179 行  
**问题说明：** `Transfer` 对 from account 做 `IsAuthorized`，但 spot 分支给 `ToAccountIndex` 加余额时只调用 `AddAccountAssetBalance`。`GetAccountAsset` 在缺行时直接返回默认 row，不验证 account 是否存在。  
**可能后果：** 用户可以把 spot asset 转到任意不存在的 account index，形成 orphan `AccountAsset`。这会污染状态、破坏 account asset 与 account 主表的外键关系，也可能让未来创建到该 index 的账户意外继承资产。  
**修改建议：** `Transfer` 应先 `GetAccount(msg.ToAccountIndex)`。更稳妥的是提供 `AddAccountAssetBalanceStrict`，对所有外部 Msg 路径强制 account 存在，只有 migration/genesis 或特定内部路径可跳过。  
**建议代码：**

```go
if _, err := m.GetAccount(ctx, msg.ToAccountIndex); err != nil {
	return nil, err
}
```

### 问题 7：leverage/margin config 接口写入了未验证且未被风控使用的字段

**严重程度：** Medium  
**位置：** `x/account/keeper/msg_server.go` `UpdateAccountAssetConfig` 第 210-215 行、`UpdateLeverage` 第 333-342 行；`x/account/types/msgs.go` `MsgUpdateLeverage.ValidateBasic` 第 98 行；`x/risk/keeper/keeper.go` `ComputeRiskInfo` 第 106-113 行  
**问题说明：** `UpdateLeverage` 不校验 `NewMarginMode` enum，不校验 market 是否存在，也不校验 `NewInitialMarginFraction` 是否满足 market 的 min/default margin chain。更关键的是 risk 计算使用 `MarketDetails.DefaultInitialMarginFraction`，没有使用 position 上的 `InitialMarginFraction`，所以该 Msg 名义上更新 leverage，实际上风控不生效。`UpdateAccountAssetConfig` 同样直接写入 `NewMarginMode`，未做 enum 校验。  
**可能后果：** 非法 margin mode 会落到 cross 分支或未来逻辑的未知分支；用户或客户端以为 leverage 已生效，但真实 IM 仍按 market default 计算。若后续模块开始信任这些字段，会引入隐蔽兼容性风险。  
**修改建议：** 明确 `InitialMarginFraction` 的业务语义。如果要支持账户级 leverage，risk 应使用 position IMF，并在 Msg 层校验 market 存在、mode 合法、无 open orders、IMF 在合法范围内。如果暂不支持，应删除或禁用该 Msg，避免伪配置。  

### 问题 8：`EnsureMasterAccount` 忽略查找 master 时的所有错误

**严重程度：** Medium  
**位置：** `x/account/keeper/account.go` `EnsureMasterAccount` 第 56-63 行  
**问题说明：** `EnsureMasterAccount` 只在 `GetMasterAccountByOwner` 返回 nil error 时返回已有账户，其他错误全部被当作“不存在”处理并继续分配新 master index。这里没有区分 `ErrAccountNotFound`、OwnerToIndex 指向缺失 account、codec/store 错误或其他异常。  
**可能后果：** 一旦 owner index 状态不一致，deposit auto-create 可能创建第二个 master account 并覆盖 `OwnerToIndex`，旧账户被孤立；如果底层 store/codec 错误被吞掉，还会扩大状态损坏范围。  
**修改建议：** 只在确定是 owner mapping 不存在时创建新 master。OwnerToIndex 存在但 account 缺失应视为状态损坏并返回错误。  
**建议代码：**

```go
a, err := k.GetMasterAccountByOwner(ctx, bech)
if err == nil {
	return a, nil
}
if !errors.Is(err, types.ErrAccountNotFound) {
	return types.Account{}, err
}
```

### 问题 9：account genesis 校验不足，无法防止无效状态进入链

**严重程度：** Medium  
**位置：** `x/account/types/genesis.go` `Validate` 第 38-49 行；`x/account/keeper/genesis.go` `InitGenesis` 第 9-39 行  
**问题说明：** Genesis 只检查重复 `account_index`，不校验 counters 范围、master/sub index 区间、owner 唯一性、account type enum、trading mode enum、collateral/position/asset balance 是否 nil 或负数、`PublicPoolInfo` 是否与 account type 匹配、insurance fund 固定账户不变量、`AccountAssets` 和 `AccountPositions` 的重复 key 或外键存在性。  
**可能后果：** 自定义 genesis、state migration 或手工修复可以把非法账户、负 spot balance、错误 pool shares、重复 asset rows 带入运行状态，后续 keeper 逻辑会在隐含假设下继续处理，造成资金和风险计算异常。  
**修改建议：** 把 account 状态不变量集中到 `Validate` 或 keeper-level `ValidateGenesis`，覆盖账户索引区间、枚举、外键、pool info、shares list 长度、策略数组长度、计数器不回退等。  

## 3. 边界条件和异常路径

| 场景 | 当前代码行为 | 风险 | 建议 |
|---|---|---|---|
| public pool operator 调用 `Withdraw(poolIdx, USDC, RouteTypePerps)` | `IsAuthorized` 通过，直接扣 pool collateral 并给 operator 打款 | 绕过 share burn，LP 资金可被抽走 | 普通 account Msg 拒绝 pool/IF account |
| public pool operator 调用 `Transfer(poolIdx -> master)` | margin-enabled asset 分支直接移动 collateral | pool NAV 和 shares 不一致 | 同上，pool 资金只走专用路径 |
| 非 margin asset 用 `RouteTypePerps` withdraw | 扣通用 collateral，发送该 asset denom | 可提走别人的 spot denom 储备 | withdraw 增加与 deposit 对称的 margin guard |
| 有未结算 funding 的 open position 后提现 | account 路径不 settle funding，risk 用 stale `EntryQuote` | 亏损 funding 未落账时可转出资金 | 提现/转账前 settle 全部非零 position funding |
| isolated position remove margin 前有 pending funding | 只检查旧 allocated margin 和旧 uPnL | 可能错误放行移除保证金 | 先 settle 目标 market funding，再更新 margin |
| `riskKeeper == nil` | `Withdraw`/`Transfer`/`UpdateMargin` 跳过风险检查 | 自定义 app wiring 下资金操作无风控 | 资金减少路径必须 fail closed |
| operator burn 到 `operator_shares == 0` | invariant 检查被 `IsPositive()` 跳过 | skin-in-the-game 约束失效 | 非 frozen pool 始终检查 min rate |
| `ExportGenesis` 后重新 Init | assets/positions/metas 不在 exported genesis 中 | 仓位和 spot 余额丢失 | 完整导出所有 account 子状态 |
| spot transfer 到不存在 account | 自动创建 `AccountAsset` row | orphan state，未来账户可能继承资产 | to account 必须存在 |
| `UpdateLeverage` 传非法 margin mode | 直接写入 position | 风险逻辑按 cross 默认分支处理未知 mode | 校验 enum，未知值拒绝 |
| `UpdateLeverage` 传不存在 market | 对不存在 market 写零 position config | 状态污染，后续语义不清 | 读取 market details 并校验 |
| `InitialMarginFraction` 被更新后交易 | risk 仍使用 market default IMF | 用户配置与真实风控不一致 | 接入 risk 或禁用该字段 |
| `PublicPoolShares` 列表满后 mint | 资金和 pool shares 已先修改，随后返回错误 | 依赖 tx rollback，内部调用容易踩坑 | 先做 shares list capacity preflight |
| `EnsureMasterAccount` 遇到非 NotFound 错误 | 继续创建新 master | 可能重复 master 或覆盖 owner index | 精确区分错误类型 |
| genesis 中 `AccountAsset` 重复 key | Validate 不检查，Init 后后写覆盖 | 导入状态不可审计 | genesis 校验重复 key |
| 查询大量 subaccounts/assets/positions | 多处无 pagination 或忽略 pagination | gRPC 查询性能和内存风险 | 使用 Cosmos pagination helper |

## 4. 安全性审查

权限校验的最大问题是 account role 没有进入普通 Msg 的权限边界。`IsAuthorized` 对 master/sub 的简单 owner 匹配可以接受，但 public pool account 拥有同一个 `OwnerAddress` 后，普通资金操作也被授权，这是 pool 资金模型的破口。建议把 “普通账户可操作” 与 “pool operator 可操作” 分成两个显式 helper，而不是复用一个 `IsAuthorized`。

资产守恒方面，`Withdraw` 的 route 校验不对称会造成 denom 级别资产错配。perps collateral 是一个通用 bucket，但 bank module 实际发送的是具体 asset denom；如果不强制只能提现 canonical margin denom，就会把 collateral 会计和 bank denom 会计拆开。

风控方面，account 模块过度依赖调用顺序假设：它持有 `fundingKeeper`，但没有在资金减少前 settle funding；它持有 `riskKeeper`，但 nil 时选择跳过风险检查。资金路径应 fail closed，尤其是链上账户系统不能把 “app 一定正确 wiring” 当作安全边界。

public pool 安全方面，份额守恒主要依赖 `TotalShares`、`OperatorShares`、用户 `PublicPoolShares` 和 pool collateral 同步更新。目前普通 account Msg 可以绕过这套同步，operator burn 也能绕过最小 operator share rate 的归零边界。这里需要增加 invariant tests：`sum(user shares) + operator_shares == total_shares`，以及 pool collateral 变化只能来自允许路径。

精度和 rounding 方面，share 计算整体使用整数向下取整，方向基本偏向 pool 留 dust，但缺少边界测试。应补充 tiny mint 返回 0、partial burn principal rounding、operator fee shares rounding、`Uint64()` 响应溢出边界等测试。

并发和重入方面，Cosmos 单线程执行降低了重入风险，但同一 tx 内多 Msg 顺序仍可能利用 stale funding 或 pool 直接转账路径。重复提交不是主要问题，但 force burn、burn shares、withdraw 都不是幂等接口，应在测试中断言失败路径不残留部分状态。

## 5. 代码规范和可维护性

- `fundingKeeper` 字段被注入但 account Msg 没有使用，属于很强的误导信号。要么实现 settle 逻辑，要么删除字段并把 pending funding 放入 risk 计算。
- `riskKeeper != nil` 时才检查风险的写法不适合资金模块。public pool NAV helper 已经在 nil 时返回错误，普通提现/转账也应一致 fail closed。
- `MsgCreatePublicPool` 的注释仍说 canonical IF 分支会 forced 到 `INSURANCE_FUND + UNIFIED`，但代码第 34-44 行明确拒绝非 `PUBLIC_POOL`。proto 注释也保留了旧语义，建议同步接口文档，避免客户端误用。
- `UpdateLeverage` 目前像“写配置”，但函数名和 Msg 名暗示会影响风控。建议把风控可见字段和纯展示字段分开，或增加注释明确未生效。
- `UpdateAccountConfig`、`UpdateAccountAssetConfig`、`UpdateLeverage` 没有统一使用 `IsAuthorized` 和 role guard，权限风格不一致。建议建立 `getAuthorizedUserAccount(ctx, signer, idx)` 一类 helper。
- 查询层 `SubAccounts` 的 proto 有 pagination 字段，但实现直接全量遍历并忽略 pagination。`AccountAssets`、`AccountPositions` 也没有分页，状态增长后容易变成慢查询。
- `GenesisState.Validate` 与 keeper 的运行时不变量差距太大。建议把 account invariant 写成可复用 validator，genesis、migration、测试都调用同一套逻辑。

## 6. 测试建议

| 优先级 | 测试场景 | 为什么需要 |
|---|---|---|
| P0 | 创建 public pool，非 operator LP mint 后，operator 直接 `MsgWithdraw` pool collateral，应失败 | 防止 pool 资金绕过 share burn 被抽走 |
| P0 | 创建 public pool 后，operator `MsgTransfer(poolIdx -> master)`，应失败 | 覆盖内部 collateral 转账绕过路径 |
| P0 | 用户有 USDC collateral，另一个用户 deposit 非 margin spot asset 后，前者用 `RouteTypePerps` withdraw 该 asset，应失败 | 防止 cross-denom 资产盗取 |
| P0 | 有未结算 funding 亏损的账户执行 withdraw/transfer，应先 settle 并按 settle 后风险拒绝或放行 | 覆盖 stale funding 风险 |
| P0 | operator 在存在非 operator shares 且 `min_operator_share_rate > 0` 时 burn 到 0，应失败 | 覆盖 operator share rate 归零边界 |
| P0 | `ExportGenesis -> InitGenesis` round trip 后 spot balances、positions、metas 完全一致 | 防止导出迁移丢状态 |
| P1 | spot transfer 到不存在 account index 应失败且不写 `AccountAsset` | 防止 orphan asset row |
| P1 | `UpdateLeverage` 非法 margin mode、非法 market、低于市场 min IMF 均失败 | 防止非法配置落库 |
| P1 | isolated position 有 pending funding 后 remove margin，断言 funding 已结算且风险用新状态 | 覆盖 isolated margin 资金路径 |
| P1 | `riskKeeper` 未 wire 的 keeper 调用提现/转账，应返回内部错误 | 防止自定义 app 或测试绕过风控 |
| P1 | public pool share tiny mint、partial burn、operator fee rounding、principal rounding | 覆盖份额精度和 dust |
| P2 | genesis 中重复 account asset key、重复 position key、非法 account type、pool info 缺失或多余均失败 | 防止迁移状态污染 |
| P2 | `SubAccounts` pagination limit/offset 生效 | 防止查询层随账户数线性爆内存 |
| P2 | `MsgUpdateAccountAssetConfig` 非法 margin mode 失败 | 防止 enum 污染 |

## 7. 推荐修改摘要

- [ ] 必须修改的问题：禁止 public pool/IF 使用普通 account Msg；`Withdraw` perps route 增加 margin asset 校验；资金减少前 settle pending funding；operator burn 始终检查 min operator share rate；补齐 `ExportGenesis`。
- [ ] 建议修改的问题：spot transfer 校验收款 account 存在；`riskKeeper` nil 时 fail closed；修复 `EnsureMasterAccount` 错误处理；统一 account role/authorization helper。
- [ ] 建议补充的测试：P0/P1 表格中的 pool drain、cross-denom withdraw、funding stale、operator shares、genesis round-trip、非法 enum 和 rounding 测试。
- [ ] 可以后续优化的问题：查询 pagination、genesis invariant 复用、proto 注释同步、shares list full 的 preflight 顺序、account-level leverage 语义澄清。

验证：本次审查期间运行 `go test ./...`，现有测试全部通过；上述问题主要来自静态业务逻辑审查和现有测试未覆盖的对抗路径。
