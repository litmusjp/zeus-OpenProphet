// Build realized trades from broker-confirmed filled orders.
// Orders are matched FIFO per symbol; unmatched/open lots are not assigned P/L.

function field(order, upper, lower) {
  return order?.[upper] ?? order?.[lower];
}

function number(value) {
  const n = Number(value);
  return Number.isFinite(n) ? n : null;
}

function timeOf(order) {
  return field(order, 'FilledAt', 'filledAt') || field(order, 'SubmittedAt', 'submittedAt') || null;
}

function isFilled(order) {
  const status = String(field(order, 'Status', 'status') || '').toLowerCase();
  return (status === 'filled' || status === 'partially_filled' || status === 'partially-filled')
    && number(field(order, 'FilledQty', 'filledQty')) > 0
    && number(field(order, 'FilledAvgPrice', 'filledAvgPrice')) !== null;
}

function isOption(symbol) {
  // OCC option symbols are 15 or 21 characters; the date/put-call strike suffix
  // makes this safer than assuming every long symbol is an option.
  return /^[A-Z0-9]{1,6}\d{6}[CP]\d{8}$/.test(String(symbol || '').toUpperCase());
}

export function buildTradeLedger(orders = [], metadata = {}) {
  const sorted = orders
    .filter(isFilled)
    .map(order => ({ order, time: timeOf(order), sortTime: Date.parse(timeOf(order) || '') || 0 }))
    .sort((a, b) => a.sortTime - b.sortTime);
  const lots = new Map();
  const realized = [];

  for (const { order, time } of sorted) {
    const symbol = String(field(order, 'Symbol', 'symbol') || '').toUpperCase();
    const side = String(field(order, 'Side', 'side') || '').toLowerCase();
    if (!symbol || (side !== 'buy' && side !== 'sell')) continue;
    let remaining = number(field(order, 'FilledQty', 'filledQty'));
    const price = number(field(order, 'FilledAvgPrice', 'filledAvgPrice'));
    const multiplier = isOption(symbol) ? 100 : 1;
    const queue = lots.get(symbol) || [];

    while (remaining > 0 && queue.length && queue[0].side !== side) {
      const lot = queue[0];
      const quantity = Math.min(remaining, lot.quantity);
      const pnl = lot.side === 'buy'
        ? (price - lot.price) * quantity * multiplier
        : (lot.price - price) * quantity * multiplier;
      const entryValue = lot.price * quantity * multiplier;
      realized.push({
        id: `${field(order, 'ID', 'id') || 'order'}:${lot.orderId}`,
        orderId: field(order, 'ID', 'id') || null,
        entryOrderId: lot.orderId,
        symbol,
        side,
        quantity,
        entryPrice: lot.price,
        exitPrice: price,
        pnl: Number(pnl.toFixed(8)),
        pnlPercent: entryValue ? Number(((pnl / entryValue) * 100).toFixed(4)) : null,
        entryTime: lot.time,
        exitTime: time,
        timestamp: time,
        status: 'realized',
        assetType: multiplier === 100 ? 'option' : 'stock',
        ...metadata,
      });
      remaining -= quantity;
      lot.quantity -= quantity;
      if (lot.quantity <= 1e-10) queue.shift();
    }

    if (remaining > 0) {
      queue.push({
        orderId: field(order, 'ID', 'id') || null,
        side,
        quantity: remaining,
        price,
        time,
      });
    }
    lots.set(symbol, queue);
  }

  return realized.sort((a, b) => Date.parse(b.timestamp || '') - Date.parse(a.timestamp || ''));
}
