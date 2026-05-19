# matching events

## EventOrderFilled

撮合循环每产生一个 fill leg 发一次。一次撮合产生 N legs ⇒ N 条事件。仅在 fill 被成功结算后发射；被驱逐（evicted）或被回滚（reverted）的 leg 不发。

```proto
syntax = "proto3";
package perpdex.matching.v1;
import "gogoproto/gogo.proto";

enum FillSource {
  FILL_SOURCE_UNSPECIFIED         = 0;
  FILL_SOURCE_USER                = 1;  // MsgCreateOrder 撮合路径
  FILL_SOURCE_TRIGGER_ACTIVATED   = 2;  // EndBlocker trigger 激活后撮合
  FILL_SOURCE_LIQUIDATION_PARTIAL = 3;  // 清算 MatchLiquidationOrder 路径
}

enum MarketType {
  MARKET_TYPE_UNSPECIFIED = 0;
  MARKET_TYPE_PERPS       = 1;
  MARKET_TYPE_SPOT        = 2;
}

message EventOrderFilled {
  // 全局成交 ID，跨所有市场单调递增（不保证连续）。
  uint64 trade_id = 1;

  uint32     market_index = 2;
  MarketType market_type  = 3;

  // 成交价 = maker 静止价。
  uint32 price = 4;
  // 本 leg 成交 base 数量。
  uint64 base_amount = 5;
  // base_amount * price，链端预算避免 indexer 重算。
  string quote_amount = 6 [(gogoproto.customtype) = "cosmossdk.io/math.Int", (gogoproto.nullable) = false];

  uint64 taker_order_index        = 7;
  uint64 taker_account_index      = 8;
  uint64 taker_client_order_index = 9;
  bool   taker_is_ask             = 10;
  uint32 taker_order_type         = 11;
  uint32 taker_time_in_force      = 12;
  bool   taker_reduce_only        = 13;

  uint64 maker_order_index        = 14;
  uint64 maker_account_index      = 15;
  uint64 maker_client_order_index = 16;
  bool   maker_reduce_only        = 17;

  // 签名 math.Int；fee 正值表示账户付出。
  string taker_fee = 18 [(gogoproto.customtype) = "cosmossdk.io/math.Int", (gogoproto.nullable) = false];
  string maker_fee = 19 [(gogoproto.customtype) = "cosmossdk.io/math.Int", (gogoproto.nullable) = false];
  // 仅 source = LIQUIDATION_PARTIAL 时 > 0；否则为 0。
  string liquidation_fee           = 20 [(gogoproto.customtype) = "cosmossdk.io/math.Int", (gogoproto.nullable) = false];
  uint64 liquidation_fee_recipient = 21;
  // perp = USDC asset；spot = 该市场 quote asset。
  uint32 fee_asset_index = 22;

  // 此 fill 给 taker/maker 仓位带来的已实现 PnL 增量（签名 math.Int）。spot fill 为 0。
  string taker_realized_pnl_delta = 23 [(gogoproto.customtype) = "cosmossdk.io/math.Int", (gogoproto.nullable) = false];
  string maker_realized_pnl_delta = 24 [(gogoproto.customtype) = "cosmossdk.io/math.Int", (gogoproto.nullable) = false];

  FillSource source = 25;

  uint64 taker_remaining_after = 26;
  uint32 taker_status_after    = 27;
  uint64 maker_remaining_after = 28;
  uint32 maker_status_after    = 29;

  // 此 fill 写入后 taker/maker AccountPosition.position_id。
  // flip 时为新 ID，完全平仓时沿用旧 ID。spot fill 为 0。
  // 详见 spec/events/account.md 的生命周期定义与转移规则。
  uint64 taker_position_id = 30;
  uint64 maker_position_id = 31;
}
```

**Invariants.**
- `trade_id` 跨所有 `EventOrderFilled` 与 `EventLiquidationExecutedLeg` 全局唯一且单调递增。
- `quote_amount == base_amount * price` 精确相等。
- `market_type = SPOT` ⇒ `taker_realized_pnl_delta = maker_realized_pnl_delta = liquidation_fee = 0`，且 `taker_position_id = maker_position_id = 0`。
- `source = LIQUIDATION_PARTIAL` ⇒ taker 为清算单（系统签），`liquidation_fee` 可正可零；其它 source ⇒ `liquidation_fee = 0`。
- `liquidation_fee = 0` ⇒ `liquidation_fee_recipient = 0`。
- `market_type = PERPS` ⇒ `taker_position_id != 0 && maker_position_id != 0`；
  开仓 / 加仓 / 减仓时沿用同一条生命线 ID，flip 时为新分配的 ID，完全平仓时
  payload 中携带 **被关闭的旧 ID**（与 `EventPositionClosed.position.position_id`
  保持一致），便于 indexer 用单一 join 列对齐 fill ↔ position。

---

## EventTriggerActivated

`EndBlocker` 中触发条件满足、stop / take-profit 订单被激活转换为可撮合订单时，每条订单发一次。激活后立即进入撮合循环；本事件仅描述"转换"，不描述"成交"。

```proto
message EventTriggerActivated {
  uint64 order_index  = 1;
  uint32 market_index = 2;

  // 满足激活条件时刻的 MarketDetails.MarkPrice。
  uint32 mark_price    = 3;
  // 订单注册时的 trigger price。
  uint32 trigger_price = 4;

  // 激活前 OrderType（StopLoss / StopLossLimit / TakeProfit / TakeProfitLimit）。
  uint32 prev_order_type = 5;
  // 激活后 OrderType（Limit / Market）。
  uint32 new_order_type    = 6;
  uint32 new_time_in_force = 7;
  // 激活后 Price；Market 路径为 0。
  uint32 new_price = 8;
}
```

**Invariants.**
- `prev_order_type ∈ {StopLoss, StopLossLimit, TakeProfit, TakeProfitLimit}`。
- `new_order_type ∈ {Limit, Market}`。
- `new_order_type = Market` ⇒ `new_price = 0`。
- 对同一 `order_index` 至多发一次；激活后该订单进入终态（Filled / Cancelled / Expired），不会再次触发。
