import { timingSafeEqual } from 'crypto';

function sameSecret(expected, actual) {
  if (typeof expected !== 'string' || typeof actual !== 'string') return false;
  const expectedBytes = Buffer.from(expected);
  const actualBytes = Buffer.from(actual);
  return expectedBytes.length === actualBytes.length && timingSafeEqual(expectedBytes, actualBytes);
}

function parseBasic(value) {
  if (typeof value !== 'string' || !value.startsWith('Basic ')) return null;
  try {
    const decoded = Buffer.from(value.slice(6), 'base64').toString('utf8');
    const separator = decoded.indexOf(':');
    if (separator < 0) return null;
    return { user: decoded.slice(0, separator), pass: decoded.slice(separator + 1) };
  } catch {
    return null;
  }
}

function isHealthRequest(req) {
  const requestPath = typeof req.originalUrl === 'string'
    ? req.originalUrl.split('?')[0]
    : req.path;
  return requestPath === '/api/health' || requestPath === '/health';
}

export function createAuthMiddleware({
  bearerToken = process.env.AGENT_AUTH_TOKEN || '',
  basicUser = process.env.BASIC_AUTH_USER || '',
  basicPass = process.env.BASIC_AUTH_PASS || '',
} = {}) {
  const configured = Boolean(bearerToken || (basicUser && basicPass));

  return (req, res, next) => {
    if (isHealthRequest(req)) return next();

    const header = req.headers?.authorization;
    const bearer = typeof header === 'string' && header.startsWith('Bearer ')
      ? header.slice(7)
      : '';
    const basic = parseBasic(header);
    const validBearer = bearerToken && sameSecret(bearerToken, bearer);
    const validBasic = basic && basicUser && basicPass
      && sameSecret(basicUser, basic.user)
      && sameSecret(basicPass, basic.pass);

    if (configured && (validBearer || validBasic)) return next();

    res.setHeader('WWW-Authenticate', 'Basic realm="OpenProphet Dashboard", Bearer');
    return res.status(401).send(configured ? 'Authentication required.' : 'Authentication is not configured.');
  };
}

export function assertProductionAuthConfigured({
  nodeEnv = process.env.NODE_ENV,
  railwayEnv = process.env.RAILWAY_ENVIRONMENT_NAME || process.env.RAILWAY_ENVIRONMENT,
  bearerToken = process.env.AGENT_AUTH_TOKEN || '',
  basicUser = process.env.BASIC_AUTH_USER || '',
  basicPass = process.env.BASIC_AUTH_PASS || '',
} = {}) {
  const production = nodeEnv === 'production' || Boolean(railwayEnv && railwayEnv !== 'development');
  const configured = Boolean(bearerToken || (basicUser && basicPass));
  if (production && !configured) {
    throw new Error('Refusing to start in production without AGENT_AUTH_TOKEN or BASIC_AUTH_USER/BASIC_AUTH_PASS');
  }
}
