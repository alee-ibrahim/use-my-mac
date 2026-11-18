// Minisign verifier for the one-line installer. Stock macOS has no minisign
// and its LibreSSL cannot do Ed25519 or BLAKE2b, but every Mac has
// JavaScriptCore through `osascript -l JavaScript` (no UI, no extra tools).
//
//   osascript -l JavaScript verify.js <pubkey-b64> <trusted-comment> <hex(SHA256SUMS)> <hex(SHA256SUMS.minisig)>
//
// Prints "ok" or fails. Pure JS with BigInt (macOS 11+); also runs under
// node for tests. Mirrors internal/selfupdate/minisign.go: key ID match,
// prehashed ("ED", BLAKE2b-512) or legacy ("Ed") signature, the global
// signature over signature || trusted comment, and the exact trusted comment.
"use strict"

const M64 = (1n << 64n) - 1n
const P = (1n << 255n) - 19n
const L = (1n << 252n) + 27742317777372353535851937790883648493n

function mod(a, m) {
  const r = a % m
  return r < 0n ? r + m : r
}
function pow(b, e, m) {
  let r = 1n
  b = mod(b, m)
  while (e > 0n) {
    if (e & 1n) r = (r * b) % m
    b = (b * b) % m
    e >>= 1n
  }
  return r
}
function iroot(n, k) {
  // floor(n^(1/k)) by Newton's method
  let x = 1n << BigInt(Math.ceil(n.toString(2).length / k) + 1)
  for (;;) {
    const y = ((BigInt(k) - 1n) * x + n / x ** (BigInt(k) - 1n)) / BigInt(k)
    if (y >= x) return x
    x = y
  }
}
const PRIMES = []
for (let n = 2; PRIMES.length < 80; n++) if (PRIMES.every((p) => n % p !== 0)) PRIMES.push(n)
// SHA-512 constants: fractional parts of square / cube roots of the primes.
// The SHA-512 IV is also the BLAKE2b IV.
const IV = PRIMES.slice(0, 8).map((p) => iroot(BigInt(p) << 128n, 2) & M64)
const K = PRIMES.map((p) => iroot(BigInt(p) << 192n, 3) & M64)

const rotr = (x, n) => ((x >> n) | (x << (64n - n))) & M64

function sha512(msg) {
  const len = msg.length
  const padLen = (len + 17 + 127) & ~127
  const m = new Uint8Array(padLen)
  m.set(msg)
  m[len] = 0x80
  const bits = BigInt(len) * 8n
  for (let i = 0; i < 16; i++) m[padLen - 1 - i] = Number((bits >> BigInt(8 * i)) & 0xffn)
  const h = IV.slice()
  const w = new Array(80)
  for (let off = 0; off < padLen; off += 128) {
    for (let i = 0; i < 16; i++) {
      let v = 0n
      for (let j = 0; j < 8; j++) v = (v << 8n) | BigInt(m[off + i * 8 + j])
      w[i] = v
    }
    for (let i = 16; i < 80; i++) {
      const s0 = rotr(w[i - 15], 1n) ^ rotr(w[i - 15], 8n) ^ (w[i - 15] >> 7n)
      const s1 = rotr(w[i - 2], 19n) ^ rotr(w[i - 2], 61n) ^ (w[i - 2] >> 6n)
      w[i] = (w[i - 16] + s0 + w[i - 7] + s1) & M64
    }
    let [a, b, c, d, e, f, g, hh] = h
    for (let i = 0; i < 80; i++) {
      const S1 = rotr(e, 14n) ^ rotr(e, 18n) ^ rotr(e, 41n)
      const ch = (e & f) ^ (~e & M64 & g)
      const t1 = (hh + S1 + ch + K[i] + w[i]) & M64
      const S0 = rotr(a, 28n) ^ rotr(a, 34n) ^ rotr(a, 39n)
      const maj = (a & b) ^ (a & c) ^ (b & c)
      const t2 = (S0 + maj) & M64
      hh = g
      g = f
      f = e
      e = (d + t1) & M64
      d = c
      c = b
      b = a
      a = (t1 + t2) & M64
    }
    ;[a, b, c, d, e, f, g, hh].forEach((v, i) => (h[i] = (h[i] + v) & M64))
  }
  const out = new Uint8Array(64)
  h.forEach((v, i) => {
    for (let j = 0; j < 8; j++) out[i * 8 + j] = Number((v >> BigInt(56 - 8 * j)) & 0xffn)
  })
  return out
}

const SIGMA = [
  [0, 1, 2, 3, 4, 5, 6, 7, 8, 9, 10, 11, 12, 13, 14, 15],
  [14, 10, 4, 8, 9, 15, 13, 6, 1, 12, 0, 2, 11, 7, 5, 3],
  [11, 8, 12, 0, 5, 2, 15, 13, 10, 14, 3, 6, 7, 1, 9, 4],
  [7, 9, 3, 1, 13, 12, 11, 14, 2, 6, 5, 10, 4, 0, 15, 8],
  [9, 0, 5, 7, 2, 4, 10, 15, 14, 1, 11, 12, 6, 8, 3, 13],
  [2, 12, 6, 10, 0, 11, 8, 3, 4, 13, 7, 5, 15, 14, 1, 9],
  [12, 5, 1, 15, 14, 13, 4, 10, 0, 7, 6, 3, 9, 2, 8, 11],
  [13, 11, 7, 14, 12, 1, 3, 9, 5, 0, 15, 4, 8, 6, 2, 10],
  [6, 15, 14, 9, 11, 3, 0, 8, 12, 2, 13, 7, 1, 4, 10, 5],
  [10, 2, 8, 4, 7, 6, 1, 5, 15, 11, 9, 14, 3, 12, 13, 0],
]

function blake2b512(msg) {
  const h = IV.slice()
  h[0] ^= 0x01010040n // digest length 64, no key, fanout 1, depth 1
  const blocks = Math.max(1, Math.ceil(msg.length / 128))
  for (let b = 0; b < blocks; b++) {
    const last = b === blocks - 1
    const blk = new Uint8Array(128)
    blk.set(msg.subarray(b * 128, Math.min(msg.length, (b + 1) * 128)))
    const t = BigInt(last ? msg.length : (b + 1) * 128)
    const m = []
    for (let i = 0; i < 16; i++) {
      let v = 0n
      for (let j = 7; j >= 0; j--) v = (v << 8n) | BigInt(blk[i * 8 + j])
      m.push(v)
    }
    const v = h.concat(IV)
    v[12] ^= t & M64
    v[13] ^= t >> 64n
    if (last) v[14] ^= M64
    const G = (a, bb, c, d, x, y) => {
      v[a] = (v[a] + v[bb] + x) & M64
      v[d] = rotr(v[d] ^ v[a], 32n)
      v[c] = (v[c] + v[d]) & M64
      v[bb] = rotr(v[bb] ^ v[c], 24n)
      v[a] = (v[a] + v[bb] + y) & M64
      v[d] = rotr(v[d] ^ v[a], 16n)
      v[c] = (v[c] + v[d]) & M64
      v[bb] = rotr(v[bb] ^ v[c], 63n)
    }
    for (let r = 0; r < 12; r++) {
      const s = SIGMA[r % 10]
      G(0, 4, 8, 12, m[s[0]], m[s[1]])
      G(1, 5, 9, 13, m[s[2]], m[s[3]])
      G(2, 6, 10, 14, m[s[4]], m[s[5]])
      G(3, 7, 11, 15, m[s[6]], m[s[7]])
      G(0, 5, 10, 15, m[s[8]], m[s[9]])
      G(1, 6, 11, 12, m[s[10]], m[s[11]])
      G(2, 7, 8, 13, m[s[12]], m[s[13]])
      G(3, 4, 9, 14, m[s[14]], m[s[15]])
    }
    for (let i = 0; i < 8; i++) h[i] ^= v[i] ^ v[i + 8]
  }
  const out = new Uint8Array(64)
  h.forEach((x, i) => {
    for (let j = 0; j < 8; j++) out[i * 8 + j] = Number((x >> BigInt(8 * j)) & 0xffn)
  })
  return out
}

// Ed25519 (RFC 8032 reference algorithm, extended coordinates).
const D = mod(-121665n * pow(121666n, P - 2n, P), P)
const SQRT_M1 = pow(2n, (P - 1n) / 4n, P)

function leInt(b) {
  let x = 0n
  for (let i = b.length - 1; i >= 0; i--) x = (x << 8n) | BigInt(b[i])
  return x
}
function recoverX(y, sign) {
  if (y >= P) return null
  const x2 = mod((y * y - 1n) * pow(D * y * y + 1n, P - 2n, P), P)
  if (x2 === 0n) return sign ? null : 0n
  let x = pow(x2, (P + 3n) / 8n, P)
  if (mod(x * x - x2, P) !== 0n) x = (x * SQRT_M1) % P
  if (mod(x * x - x2, P) !== 0n) return null
  if (Number(x & 1n) !== sign) x = P - x
  return x
}
function decompress(b) {
  if (b.length !== 32) return null
  let y = leInt(b)
  const sign = Number(y >> 255n)
  y &= (1n << 255n) - 1n
  const x = recoverX(y, sign)
  if (x === null) return null
  return [x, y, 1n, (x * y) % P]
}
function add(p, q) {
  const A = mod((p[1] - p[0]) * (q[1] - q[0]), P)
  const B = mod((p[1] + p[0]) * (q[1] + q[0]), P)
  const C = mod(2n * p[3] * q[3] * D, P)
  const Dd = mod(2n * p[2] * q[2], P)
  const E = B - A
  const F = Dd - C
  const Gg = Dd + C
  const H = B + A
  return [mod(E * F, P), mod(Gg * H, P), mod(F * Gg, P), mod(E * H, P)]
}
function mul(s, p) {
  let q = [0n, 1n, 1n, 0n]
  while (s > 0n) {
    if (s & 1n) q = add(q, p)
    p = add(p, p)
    s >>= 1n
  }
  return q
}
function equal(p, q) {
  return mod(p[0] * q[2] - q[0] * p[2], P) === 0n && mod(p[1] * q[2] - q[1] * p[2], P) === 0n
}
const GY = mod(4n * pow(5n, P - 2n, P), P)
const BASE = [recoverX(GY, 0), GY, 1n, mod(recoverX(GY, 0) * GY, P)]

function concat(...parts) {
  const out = new Uint8Array(parts.reduce((n, p) => n + p.length, 0))
  let o = 0
  for (const p of parts) {
    out.set(p, o)
    o += p.length
  }
  return out
}
function ed25519Verify(pub, msg, sig) {
  if (sig.length !== 64) return false
  const A = decompress(pub)
  const R = decompress(sig.subarray(0, 32))
  if (!A || !R) return false
  const s = leInt(sig.subarray(32))
  if (s >= L) return false
  const h = leInt(sha512(concat(sig.subarray(0, 32), pub, msg))) % L
  return equal(mul(s, BASE), add(R, mul(h, A)))
}

// Encoding helpers.
function fromHex(s) {
  if (!/^([0-9a-f]{2})*$/i.test(s)) throw new Error("bad hex input")
  const out = new Uint8Array(s.length / 2)
  for (let i = 0; i < out.length; i++) out[i] = parseInt(s.substr(i * 2, 2), 16)
  return out
}
const B64 = "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789+/"
function fromB64(s) {
  s = s.trim()
  if (!/^[A-Za-z0-9+/]*={0,2}$/.test(s) || s.length % 4 !== 0) throw new Error("bad base64")
  const body = s.replace(/=+$/, "")
  const out = []
  let acc = 0
  let n = 0
  for (const ch of body) {
    acc = (acc << 6) | B64.indexOf(ch)
    n += 6
    if (n >= 8) {
      n -= 8
      out.push((acc >> n) & 0xff)
    }
  }
  return new Uint8Array(out)
}
function ascii(s) {
  const out = new Uint8Array(s.length)
  for (let i = 0; i < s.length; i++) {
    const c = s.charCodeAt(i)
    if (c > 0x7e) throw new Error("non-ASCII trusted comment")
    out[i] = c
  }
  return out
}
function text(b) {
  let s = ""
  for (const c of b) s += String.fromCharCode(c)
  return s
}
function same(a, b) {
  return a.length === b.length && a.every((x, i) => x === b[i])
}

function run(argv) {
  if (typeof BigInt === "undefined") throw new Error("this macOS is too old (no BigInt)")
  if (argv.length !== 4) throw new Error("usage: verify.js <pubkey> <trusted-comment> <hex-file> <hex-minisig>")
  const pk = fromB64(argv[0])
  if (pk.length !== 42 || pk[0] !== 0x45 || pk[1] !== 0x64) throw new Error("malformed public key")
  const keyID = pk.subarray(2, 10)
  const key = pk.subarray(10)
  const msg = fromHex(argv[2])
  const lines = text(fromHex(argv[3])).replace(/\r/g, "").split("\n")
  if (lines.length < 4 || !lines[0].startsWith("untrusted comment:")) throw new Error("malformed signature file")
  const sl = fromB64(lines[1])
  if (sl.length !== 74) throw new Error("malformed signature line")
  if (!lines[2].startsWith("trusted comment: ")) throw new Error("missing trusted comment")
  const tc = lines[2].slice("trusted comment: ".length)
  const global = fromB64(lines[3])
  if (!same(sl.subarray(2, 10), keyID)) throw new Error("signed with a different key")
  const sig = sl.subarray(10)
  let signed
  if (sl[0] === 0x45 && sl[1] === 0x44) signed = blake2b512(msg) // "ED"
  else if (sl[0] === 0x45 && sl[1] === 0x64) signed = msg // "Ed"
  else throw new Error("unsupported signature algorithm")
  if (!ed25519Verify(key, signed, sig)) throw new Error("signature verification failed")
  if (!ed25519Verify(key, concat(sig, ascii(tc)), global)) throw new Error("trusted comment signature verification failed")
  if (tc !== argv[1]) throw new Error("signature is for '" + tc + "', expected '" + argv[1] + "'")
  return "ok"
}

// node (tests): same entry point as osascript's run(argv).
if (typeof process !== "undefined" && process.argv) {
  try {
    console.log(run(process.argv.slice(2)))
  } catch (e) {
    console.error("error: " + e.message)
    process.exit(1)
  }
}
