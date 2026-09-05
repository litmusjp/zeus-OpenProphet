// Chat History Store - Persists conversations per account/session
// JSONL format for efficient append-only writes
import fs from 'fs/promises';
import fsSync from 'fs';
import path from 'path';
import { fileURLToPath } from 'url';

const __dirname = path.dirname(fileURLToPath(import.meta.url));
const DATA_DIR = path.join(__dirname, '..', 'data');

export class ChatStore {
  constructor(dataDir = DATA_DIR) {
    this.dataDir = dataDir;
    this._writeQueues = new Map(); // accountId -> Promise chain
  }

  // ── Paths ────────────────────────────────────────────────────────

  _sandboxDir(accountId) {
    return path.join(this.dataDir, 'sandboxes', accountId);
  }

  _chatDir(accountId) {
    return path.join(this._sandboxDir(accountId), 'chat-history');
  }

  _sessionFile(accountId, sessionId) {
    return path.join(this._chatDir(accountId), `session-${sessionId}.jsonl`);
  }

  _sessionIndexFile(accountId) {
    return path.join(this._chatDir(accountId), 'sessions.json');
  }

  // ── Ensure dirs ──────────────────────────────────────────────────

  async _ensureDirs(accountId) {
    await fs.mkdir(this._chatDir(accountId), { recursive: true });
  }

  // ── Write queue (serialize writes per account) ───────────────────

  _enqueue(accountId, fn) {
    const prev = this._writeQueues.get(accountId) || Promise.resolve();
    const next = prev.then(fn).catch(err => {
      console.error(`ChatStore write error (${accountId}):`, err.message);
    });
    this._writeQueues.set(accountId, next);
    return next;
  }

  // ── Session Index ────────────────────────────────────────────────

  async _loadSessionIndex(accountId) {
    try {
      const raw = await fs.readFile(this._sessionIndexFile(accountId), 'utf-8');
      return JSON.parse(raw);
    } catch {
      return { sessions: [] };
    }
  }

  async _saveSessionIndex(accountId, index) {
    await this._ensureDirs(accountId);
    await fs.writeFile(this._sessionIndexFile(accountId), JSON.stringify(index, null, 2));
  }

  // ── Public API ───────────────────────────────────────────────────

  /**
   * Start or resume a session. Creates index entry if new.
   * @param {string} accountId
   * @param {string} sessionId - OpenCode session ID
   * @param {object} metadata - { agentName, model, ... }
   */
  async startSession(accountId, sessionId, metadata = {}) {
    return this._enqueue(accountId, async () => {
      await this._ensureDirs(accountId);
      const index = await this._loadSessionIndex(accountId);

      const existing = index.sessions.find(s => s.id === sessionId);
      if (existing) {
        // Resume — update lastActiveAt
        existing.lastActiveAt = new Date().toISOString();
        existing.metadata = { ...existing.metadata, ...metadata };
      } else {
        // New session
        index.sessions.unshift({
          id: sessionId,
          createdAt: new Date().toISOString(),
          lastActiveAt: new Date().toISOString(),
          messageCount: 0,
          metadata,
        });
      }

      // Keep max 100 sessions in index
      if (index.sessions.length > 100) {
        index.sessions = index.sessions.slice(0, 100);
      }

      await this._saveSessionIndex(accountId, index);
    });
  }

  /**
   * Append a message to a session.
   * @param {string} accountId
   * @param {string} sessionId
   * @param {object} message - { role, content, beat?, toolCalls?, cost?, tokens? }
   */
  async addMessage(accountId, sessionId, message) {
    return this._enqueue(accountId, async () => {
      await this._ensureDirs(accountId);

      const entry = {
        timestamp: new Date().toISOString(),
        ...message,
      };

      const filePath = this._sessionFile(accountId, sessionId);
      await fs.appendFile(filePath, JSON.stringify(entry) + '\n');

      // Update session index message count
      const index = await this._loadSessionIndex(accountId);
      const session = index.sessions.find(s => s.id === sessionId);
      if (session) {
        session.messageCount = (session.messageCount || 0) + 1;
        session.lastActiveAt = new Date().toISOString();
        // Store last message preview
        if (message.content) {
          session.lastMessage = message.content.substring(0, 120);
        }
        await this._saveSessionIndex(accountId, index);
      }
    });
  }

  /**
   * List all sessions for an account (newest first).
   * @param {string} accountId
   * @param {number} limit
   * @returns {Promise<Array>}
   */
  async listSessions(accountId, limit = 50) {
    const index = await this._loadSessionIndex(accountId);
    return index.sessions.slice(0, limit);
  }

  /**
   * Get all messages for a session.
   * @param {string} accountId
   * @param {string} sessionId
   * @param {object} opts - { offset, limit }
   * @returns {Promise<Array>}
   */
  async getSessionMessages(accountId, sessionId, opts = {}) {
    const { offset = 0, limit = 500 } = opts;
    const filePath = this._sessionFile(accountId, sessionId);

    try {
      const raw = await fs.readFile(filePath, 'utf-8');
      const lines = raw.trim().split('\n').filter(Boolean);
      const messages = lines.map(line => {
        try { return JSON.parse(line); } catch { return null; }
      }).filter(Boolean);

      return messages.slice(offset, offset + limit);
    } catch (err) {
      if (err.code === 'ENOENT') return [];
      throw err;
    }
  }

  /**
   * Get a session's metadata from the index.
   * @param {string} accountId
   * @param {string} sessionId
   * @returns {Promise<object|null>}
   */
  async getSession(accountId, sessionId) {
    const index = await this._loadSessionIndex(accountId);
    return index.sessions.find(s => s.id === sessionId) || null;
  }

  /** Return a compact context window for a restarted agent or manager. */
  async getRecentContext(accountId, opts = {}) {
    const { sessionLimit = 5, messagesPerSession = 8, charLimit = 12000 } = opts;
    const sessions = await this.listSessions(accountId, sessionLimit);
    const context = [];
    let chars = 0;
    for (const session of sessions) {
      const messages = await this.getSessionMessages(accountId, session.id, { limit: messagesPerSession });
      const useful = messages.filter(m => m && (m.content || m.eventType || m.kind === 'tool_call'));
      if (!useful.length) continue;
      const block = {
        sessionId: session.id,
        createdAt: session.createdAt,
        lastActiveAt: session.lastActiveAt,
        metadata: session.metadata || {},
        messages: useful.map(m => ({
          timestamp: m.timestamp,
          role: m.role,
          kind: m.kind,
          eventType: m.eventType,
          content: typeof m.content === 'string' ? m.content.substring(0, 1800) : undefined,
          tool: m.tool,
          args: m.args,
          result: typeof m.result === 'string' ? m.result.substring(0, 800) : undefined,
        })),
      };
      const size = JSON.stringify(block).length;
      if (chars + size > charLimit) break;
      context.push(block);
      chars += size;
    }
    return context;
  }

  /**
   * Delete a session (messages + index entry).
   * @param {string} accountId
   * @param {string} sessionId
   */
  async deleteSession(accountId, sessionId) {
    return this._enqueue(accountId, async () => {
      // Remove file
      try {
        await fs.unlink(this._sessionFile(accountId, sessionId));
      } catch {}

      // Remove from index
      const index = await this._loadSessionIndex(accountId);
      index.sessions = index.sessions.filter(s => s.id !== sessionId);
      await this._saveSessionIndex(accountId, index);
    });
  }

  /**
   * Get all sessions across all accounts (for global search).
   * @param {number} limit
   * @returns {Promise<Array>} - [{ accountId, ...session }]
   */
  async listAllSessions(limit = 100) {
    const sandboxDir = path.join(this.dataDir, 'sandboxes');
    let accountIds = [];
    try {
      accountIds = await fs.readdir(sandboxDir);
    } catch {
      return [];
    }

    const allSessions = [];
    for (const accountId of accountIds) {
      const sessions = await this.listSessions(accountId);
      for (const session of sessions) {
        allSessions.push({ accountId, ...session });
      }
    }

    // Sort by lastActiveAt descending
    allSessions.sort((a, b) => new Date(b.lastActiveAt) - new Date(a.lastActiveAt));
    return allSessions.slice(0, limit);
  }
}

export default ChatStore;
