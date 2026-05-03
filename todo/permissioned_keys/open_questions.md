# 待决问题清单

后续如要正式开干，需先回答以下问题。问题之间有依赖关系，按顺序解。

## Q1（最高优先级）— 架构方向：A / B / C

详见 [`alternatives.md`](./alternatives.md)。

- **A** 完整移植 dydx `x/accountplus`：~3000-5000 LoC，最规范，扩展能力最强。
- **B** 精简版 dydx 风格 perpdex `x/permissioned_keys`：~1500-2000 LoC，trader 无链上地址（**当前推荐**）。
- **C** zk-dex 风格 + cosmos 派生地址：~1000-1500 LoC，最小改动但需 prefix + SendRestriction。

回答 Q1 后，下面的子问题集合也相应不同。

## Q2（如果选 A 或 B）— scope 是否要 `MessageFilter`

最小化路线下只做 `SignatureVerification + SubaccountFilter`（trader 能以某 account 身份发任意业务 Msg）。如果要进一步限制 trader 只能发某些 grpc msg type（如只能 `MsgCreateOrder`，不能 `MsgWithdraw`），需要加 `MessageFilter` 子节点。

- 是 → 增加一个 authenticator 子节点 + `MsgAddAuthenticator` 携带 message pattern config
- 否 → 简化，未来扩展再加

## Q3（如果选 A 或 B）— TxExtension 携带的字段

dydx 默认只携带 `selected_authenticators: []uint64`。perpdex 需要业务 nonce 时，建议扩展为：

```proto
message PerpdexTxExtension {
  repeated uint64 selected_authenticators = 1;
  repeated uint64 api_key_nonces          = 2; // per-msg 业务 nonce
}
```

- 这样 Msg proto 完全不动（nonce 进 ext）。
- 或者：Msg 加 `nonce` 字段，ext 只放 `selected_authenticators`。需要 trade-off。

## Q4（如果选 A 或 B）— fee 扣除路径如何识别 `account_index`

trader 发的 Tx 里没有 `from_account_index` 直接字段（dydx 走 owner.Bank 不需要）。要从 `Account.Collateral` 扣 USDC，需要在 ante 阶段确定 `account_index`：

- 选项 1：从 `SubaccountFilter` 的 stored config 里取（一对一绑定）。**优点**：trader 在 Tx 里完全不用指定 account_index；**缺点**：trader 的 auth_id 与 account_index 一一对应，不能复用同一 trader 操作多个 account。
- 选项 2：让 trader 在 TxExtension 里显式指定 `fee_account_index`。**优点**：灵活；**缺点**：客户端要多带一个字段、需要校验"这个 account_index 确实在当前 auth 树的 SubaccountFilter 允许范围内"。
- 选项 3：从第一笔 Msg 的 `account_index` 字段取（perpdex 现有所有 Msg 都有 `account_index`）。**优点**：客户端无新负担；**缺点**：如果一个 Tx 多个 Msg 跨 account，处理需谨慎（混合 reject）。

## Q5（如果选 C）— prefix `pxapi` 是否够

业务展示层用 `pxapi1...` 视觉差异是否够？还是真的要 fork cosmos sdk 的 bech32 codec 让链上 wire 也用独立 prefix？

- 是（够） → 走当前方案稿（[`proposal_zkdex_style.md`](./proposal_zkdex_style.md)）
- 否（要底层 fork） → 工作量翻 2-3 倍、阻塞 cosmos SDK 升级，**不推荐**，需另立专题

## Q6 — 客户端 SDK 范围

无论选 A/B/C，都需要给客户端提供：

- 注册 / 撤销 / 轮转 API key 的请求构造（gRPC + CLI）
- 用 trader 私钥构造 / 签名 Tx 的工具（A、B 需要 TxExtension 注入；C 可直接用 cosmos 标准）
- 钱包集成 ：是只支持 perpd CLI 还是要 typescript / python SDK ?

需明确：
- 是否同时给 TS / Python 提供？
- 是否要更新 `tests/e2e/msg/*` helper 让 e2e 走真 ante（之前所有 e2e 都直调 msg_server 绕过 ante）？

## Q7 — 治理 / 模块参数

需要哪些 governance-only 参数？

- `MaxApiKeysPerAccount`（防滥用，参考 zk-dex `MAX_API_KEY_INDEX = 254`）
- `ApiKeyFeeDenom`（默认 USDC）
- `ApiKeyMinNonceGap`（防压力测试时 nonce 反压）
- 模块开关 `IsApiKeyActive`（circuit breaker，参考 osmosis smart-account）

是否走 `MsgUpdateParams` + gov flow？

## Q8 — 与 indexer / 区块浏览器 的兼容

- API key 注册 / 撤销事件需要 emit 哪些字段？
- trader 发出的 Tx 在 indexer 里怎么显示？（owner / trader 双视角？）
- 历史回放（区块同步）是否能正确恢复 nonce ？（应该可以，因为 nonce ++ 是在 ante 里 commit 的 state changes）

## Q9 — 升级路径

如果先做 C，未来要升级 B/A，迁移成本预估：

- C → B：API key 从派生地址迁移到 owner 名下的 auth_id 列表，trader 私钥可保留但 chain 上 trader 无地址。预计 ~500 LoC migration + indexer 配合。
- B → A：精简 keeper 改成 dydx accountplus 全套。预计 ~2000 LoC。

是否接受未来升级成本？还是一步到位？
