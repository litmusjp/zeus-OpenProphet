import { test } from 'node:test';
import assert from 'node:assert/strict';
import { buildSystemPrompt, getOpenCodeEnvCredential, hasOpenCodeCredential, tradeEventFromToolUse } from '../agent/harness.js';
import { createGoLogLineBuffer, shouldShowGoLogLine } from '../agent/orchestrator.js';
import { buildTradeLedger } from '../agent/trade-ledger.js';

test('OpenCode authentication accepts a Zen API key without Anthropic credentials', () => {
  assert.equal(hasOpenCodeCredential('', { OPENCODE_API_KEY: 'zen-key' }), true);
  assert.equal(getOpenCodeEnvCredential({ OPENAI_API_KEY: 'openai-key' }), 'OPENAI_API_KEY');
  assert.equal(hasOpenCodeCredential('', { OPENAI_API_KEY: 'openai-key' }), true);
  assert.equal(hasOpenCodeCredential('', { OPENAI_API_KEY: '   ' }), false);
  assert.equal(hasOpenCodeCredential('', {}), false);
});

test('OpenCode authentication accepts any configured provider from auth list', () => {
  assert.equal(hasOpenCodeCredential('●  OpenCode Zen api', {}), true);
  assert.equal(hasOpenCodeCredential('●  Anthropic oauth', {}), true);
  assert.equal(hasOpenCodeCredential('●  custom-provider wellknown', {}), true);
  assert.equal(hasOpenCodeCredential('└  0 credentials', {}), false);
});

test('routine Go HTTP access logs stay out of agent terminals', () => {
  assert.equal(shouldShowGoLogLine('[GIN] 2026/09/03 - 01:25:46 | 200 | 90.261461ms | 127.0.0.1 | GET "/api/v1/account"'), false);
  assert.equal(shouldShowGoLogLine('[GIN] 2026/09/03 - 01:25:46 | 204 | 1.1ms | 127.0.0.1 | CONNECT "/stream"'), false);
  assert.equal(shouldShowGoLogLine('\x1b[32m[GIN]\x1b[0m 2026/09/03 - 01:25:46 | 302 | 1.1ms | 127.0.0.1 | TRACE "/redirect"'), false);
  assert.equal(shouldShowGoLogLine('[GIN-debug] GET /health --> healthHandler'), false);
  assert.equal(shouldShowGoLogLine('[go] [GIN-debug] GET /health --> healthHandler'), false);
  assert.equal(shouldShowGoLogLine('[go] [GIN] 2026/09/03 - 01:25:46 | 200 | 1.1ms | 127.0.0.1 | GET "/health"'), false);
  assert.equal(shouldShowGoLogLine('[GIN] 2026/09/03 - 01:25:46 | 500 | 90.261461ms | 127.0.0.1 | GET "/api/v1/account"'), true);
  assert.equal(shouldShowGoLogLine('level=info msg="Activity logging session started"'), true);
  assert.equal(shouldShowGoLogLine('[GIN] panic recovered while serving request'), true);
});

test('Go log buffering classifies complete lines instead of stream fragments', () => {
  const lines = [];
  const buffer = createGoLogLineBuffer(line => {
    if (shouldShowGoLogLine(line)) lines.push(line);
  });
  buffer.push('[GIN] 2026/09/03 - 01:25:46 | 200 | 90.2ms | 127.0.0.1 | GE');
  buffer.push('T "/api/v1/account"\nmeaningful back');
  buffer.push('end event');
  assert.deepEqual(lines, []);
  buffer.flush();
  assert.deepEqual(lines, ['meaningful backend event']);
});

test('tradeEventFromToolUse is exposed for deterministic trade telemetry', () => {
  assert.equal(typeof tradeEventFromToolUse, 'function');
});

test('tradeEventFromToolUse recognizes a stock buy execution', () => {
  assert.deepEqual(
    tradeEventFromToolUse('prophet_place_buy_order', {
      symbol: 'AAPL', quantity: 2, limit_price: 205.5,
    }),
    {
      type: 'order', tool: 'place_buy_order', symbol: 'AAPL',
      side: 'buy', quantity: 2, price: 205.5,
    },
  );
});

test('tradeEventFromToolUse ignores read-only order and position tools', () => {
  for (const tool of ['get_managed_positions', 'get_orders', 'get_options_positions']) {
    assert.equal(tradeEventFromToolUse(`prophet_${tool}`, {}), null, tool);
  }
});

test('tradeEventFromToolUse recognizes every order execution tool', () => {
  const cases = [
    ['place_sell_order', {}, 'sell'],
    ['place_options_order', { side: 'buy' }, 'buy'],
    ['place_managed_position', {}, 'buy'],
    ['close_managed_position', {}, 'sell'],
  ];
  for (const [tool, input, side] of cases) {
    const event = tradeEventFromToolUse(`prophet_${tool}`, { symbol: 'SPY', ...input });
    assert.equal(event?.tool, tool, tool);
    assert.equal(event?.side, side, `${tool} side`);
  }
});

test('trade ledger pairs filled orders and calculates realized stock and option P/L', () => {
  const trades = buildTradeLedger([
    { ID: 'b1', Symbol: 'AAPL', Side: 'buy', FilledQty: 2, FilledAvgPrice: 100, Status: 'filled', FilledAt: '2026-01-01T10:00:00Z' },
    { ID: 's1', Symbol: 'AAPL', Side: 'sell', FilledQty: 2, FilledAvgPrice: 105, Status: 'filled', FilledAt: '2026-01-01T11:00:00Z' },
    { ID: 'b2', Symbol: 'AAPL260116C00100000', Side: 'buy', FilledQty: 1, FilledAvgPrice: 2, Status: 'filled', FilledAt: '2026-01-01T12:00:00Z' },
    { ID: 's2', Symbol: 'AAPL260116C00100000', FilledQty: 1, Side: 'sell', FilledAvgPrice: 2.5, Status: 'filled', FilledAt: '2026-01-01T13:00:00Z' },
  ], { accountId: 'a1', accountName: 'Litmus 1', agentName: 'Ling' });
  assert.equal(trades.length, 2);
  assert.equal(trades[0].pnl, 50);
  assert.equal(trades[1].pnl, 10);
  assert.equal(trades[0].accountName, 'Litmus 1');
  assert.equal(trades[0].agentName, 'Ling');
  assert.equal(trades[0].assetType, 'option');
});

test('default system prompt carries the mandate, decision loop, risk discipline, and learning loop', async () => {
  const p = await buildSystemPrompt({ name: 'Prophet' }, {});
  assert.ok(p.includes('You are Prophet'), 'names the agent');
  assert.ok(p.includes('prophet_get_datetime'), 'uses the OpenCode MCP namespace');
  assert.ok(p.includes('exact registered names'), 'explains MCP tool naming');
  assert.ok(/Preserve capital/.test(p), 'states capital-preservation mandate');
  assert.ok(p.includes('## Your Heartbeat Loop'), 'has the ordered decision loop');
  assert.ok(p.includes('Risk Discipline'), 'has hard risk discipline');
  assert.ok(p.includes('GUARDRAILS'), 'points at the per-beat guardrails');
  assert.ok(p.includes('find_similar_setups') && p.includes('store_trade_setup'), 'wires recall + store');
  assert.ok(p.includes('place_buy_order'), 'includes the generated tool catalog');
});

test('custom template overrides the identity but keeps the operating instructions', async () => {
  const p = await buildSystemPrompt({ systemPromptTemplate: 'custom', customSystemPrompt: 'I am a custom bot.' }, {});
  assert.ok(p.startsWith('I am a custom bot.'), 'uses the custom identity verbatim');
  assert.ok(p.includes('## Your Heartbeat Loop'), 'still appends the system instructions');
});
