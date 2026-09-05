import test from 'node:test';
import assert from 'node:assert/strict';
import os from 'node:os';
import fs from 'node:fs/promises';
import path from 'node:path';
import { ChatStore } from '../agent/chat-store.js';

test('ChatStore preserves identity, structured events, and compact context', async () => {
  const temp = await fs.mkdtemp(path.join(os.tmpdir(), 'openprophet-chat-'));
  try {
    const store = new ChatStore(temp);
    await store.startSession('acct-1', 'session-1', {
      accountId: 'acct-1', accountName: 'Litmus1', sandboxId: 'sbx-1',
      sandboxName: 'Litmus1', agentId: 'agent-1', agentName: 'Catalyst', model: 'opencode/test',
    });
    await store.addMessage('acct-1', 'session-1', {
      role: 'assistant', kind: 'decision', eventType: 'strategy_decision',
      content: 'BUY AAPL because the trend and risk/reward meet the strategy.',
    });
    await store.addMessage('acct-1', 'session-1', {
      role: 'assistant', kind: 'tool_call', eventType: 'tool_call', tool: 'place_buy_order',
      args: { symbol: 'AAPL', quantity: 1 }, result: '{"status":"accepted"}',
    });

    const session = await store.getSession('acct-1', 'session-1');
    assert.equal(session.metadata.accountName, 'Litmus1');
    assert.equal(session.metadata.sandboxName, 'Litmus1');
    assert.equal(session.metadata.agentName, 'Catalyst');
    assert.equal(session.messageCount, 2);

    const context = await store.getRecentContext('acct-1');
    assert.equal(context.length, 1);
    assert.equal(context[0].metadata.accountName, 'Litmus1');
    assert.equal(context[0].messages.some(m => m.eventType === 'strategy_decision'), true);
    assert.equal(context[0].messages.some(m => m.tool === 'place_buy_order'), true);
  } finally {
    await fs.rm(temp, { recursive: true, force: true });
  }
});
