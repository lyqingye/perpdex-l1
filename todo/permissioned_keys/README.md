# Subaccount Permissioned Keys (perpdex-l1)

> Status: **Research / Design phase**, not yet implemented.

## 1. 背景

需求来源：把 [zk-dex/lib](https://github.com/lyqingye/zk-dex/tree/main/lib) 的 subaccount API key 机制移植到 perpdex-l1，让 master 能颁发独立 key 委托给 trader / bot 使用，而不必交出 master EOA 私钥。

调研对象：

- zk-dex/lib 的 `ApiKey { api_key_index, public_key, nonce }` 模型
- dydx v4 / Osmosis 的 `x/smart-account` (`x/accountplus`) Authenticator 框架（dydx [Permissioned Keys 文档](https://docs.dydx.xyz/interaction/permissioned-keys)）
- perpdex-l1 当前的账户 / 鉴权体系（现状）

## 2. 文档导航

- [`research.md`](./research.md) — 三侧调研结论（zk-dex/lib、perpdex 现状、dydx v4）
- [`alternatives.md`](./alternatives.md) — 三个候选实现方向对比 + 推荐
- [`proposal_zkdex_style.md`](./proposal_zkdex_style.md) — 方案 C（zk-dex 风格 + 派生地址）的详细设计稿
- [`open_questions.md`](./open_questions.md) — 待决问题清单（最终方案需先回答）

## 3. 已经达成的设计共识（可作为后续方案的硬约束）

经几轮讨论已锁定：

1. **API key 绑定粒度 = `account_index`**（master 或 sub 都行），与 zk-dex/lib 的 `(owner_account_index, api_key_index)` 槽位模型一致；不绑在 master 整体上。
2. **业务 nonce 自维护 `(account_index, api_key_index) → nonce`**，单调递增防 replay；不复用 cosmos `Account.Sequence`（与 zk-dex/lib 一致）。
3. **Gas/手续费从 API key 绑定的 subaccount `Account.Collateral` 扣除（USDC）**，不做"代付到 master"也不让 trader 自己充 gas；不引入 `x/feegrant`。
4. **业务 Msg 应有统一接口抽象**（zk-dex 的 `dispatch_tx!` 等价物），让 ante 能通用调度："谁在以哪个 (account, key) 身份、第几号 nonce 发这条 Msg"。
5. **scope / 过期 / last_used_at 走最小化**（zk-dex/lib 没有这些；后续可加 scope filter 子节点扩展）。

## 4. 待选定的关键问题

集中在 [`open_questions.md`](./open_questions.md)，其中最关键的是 **架构方向**（Q1）：

- A. 完整移植 dydx 的 `x/accountplus`（最规范，最大）
- B. **精简版 dydx 风格**：自研 perpdex `x/permissioned_keys`，trader 无链上地址（**当前推荐**）
- C. zk-dex 风格 + cosmos 派生地址（最小，但视觉上需要 `pxapi` prefix + bank SendRestriction 防误转）

选定方向后，对应方案文档需要补充 / 修订（A、B 还没有详细方案稿；C 的稿在 [`proposal_zkdex_style.md`](./proposal_zkdex_style.md) 与 `.cursor/plans/perpdex_subaccount_api_keys_*.plan.md`）。
