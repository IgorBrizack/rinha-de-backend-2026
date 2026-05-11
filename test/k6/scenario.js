import http from 'k6/http';
import { check, sleep } from 'k6';
import { Counter, Rate, Trend } from 'k6/metrics';

// Custom metrics for fraud detection quality
const approvedLegit  = new Counter('approved_legit');
const blockedFraud   = new Counter('blocked_fraud');
const falsePositives = new Counter('false_positives');
const falseNegatives = new Counter('false_negatives');
const detectionRate  = new Rate('fraud_detection_rate');

export const options = {
  stages: [
    { duration: '30s', target: 20  },  // warm-up
    { duration: '1m',  target: 50  },  // ramp up
    { duration: '2m',  target: 100 },  // sustained load
    { duration: '30s', target: 0   },  // cool down
  ],
  thresholds: {
    // Must not exceed competition hard limits
    http_req_duration: ['p(99)<2000'],
    http_req_failed:   ['rate<0.15'],
  },
};

const BASE_URL = __ENV.BASE_URL || 'http://localhost:9999';

// ──────────────────────────────────────────────
// Transaction generators
// ──────────────────────────────────────────────

function isoNow(offsetMinutes = 0) {
  return new Date(Date.now() + offsetMinutes * 60_000).toISOString().replace(/\.\d+Z$/, 'Z');
}

function randomMCC() {
  const codes = ['5411', '5912', '5812', '6011', '7011', '4111', '5999'];
  return codes[Math.floor(Math.random() * codes.length)];
}

/** Typical everyday purchase — should be approved */
function legitTransaction() {
  const amount = 20 + Math.random() * 200;
  return {
    id: `legit-${Math.random().toString(36).slice(2)}`,
    transaction: {
      amount: parseFloat(amount.toFixed(2)),
      installments: Math.floor(Math.random() * 3) + 1,
      requested_at: isoNow(),
    },
    customer: {
      avg_amount: amount * (0.8 + Math.random() * 0.6),
      tx_count_24h: Math.floor(Math.random() * 5) + 1,
      known_merchants: ['merchant-supermarket', 'merchant-pharmacy', 'merchant-gas'],
    },
    merchant: {
      id: 'merchant-supermarket',
      mcc: '5411',
      avg_amount: 150.0,
    },
    terminal: {
      is_online: false,
      card_present: true,
      km_from_home: 1.5 + Math.random() * 3,
    },
    last_transaction: {
      timestamp: isoNow(-60),
      km_from_current: 0.5 + Math.random() * 2,
    },
  };
}

/** High-risk profile — large amount, unknown merchant, rapid geo-jump */
function fraudTransaction() {
  const amount = 3000 + Math.random() * 7000;
  return {
    id: `fraud-${Math.random().toString(36).slice(2)}`,
    transaction: {
      amount: parseFloat(amount.toFixed(2)),
      installments: 12,
      requested_at: isoNow(),
    },
    customer: {
      avg_amount: 80.0,
      tx_count_24h: 15 + Math.floor(Math.random() * 5),
      known_merchants: ['merchant-supermarket'],
    },
    merchant: {
      id: `merchant-unknown-${Math.floor(Math.random() * 1000)}`,
      mcc: randomMCC(),
      avg_amount: amount * 0.9,
    },
    terminal: {
      is_online: true,
      card_present: false,
      km_from_home: 500 + Math.random() * 500,
    },
    last_transaction: {
      timestamp: isoNow(-3),
      km_from_current: 800 + Math.random() * 200,
    },
  };
}

/** No prior transaction history */
function newCustomerTransaction() {
  const amount = 50 + Math.random() * 300;
  return {
    id: `new-${Math.random().toString(36).slice(2)}`,
    transaction: {
      amount: parseFloat(amount.toFixed(2)),
      installments: 1,
      requested_at: isoNow(),
    },
    customer: {
      avg_amount: 0,
      tx_count_24h: 0,
      known_merchants: [],
    },
    merchant: {
      id: 'merchant-pharmacy',
      mcc: '5912',
      avg_amount: 80.0,
    },
    terminal: {
      is_online: false,
      card_present: true,
      km_from_home: 2.0,
    },
    last_transaction: null,
  };
}

// ──────────────────────────────────────────────
// VU entrypoint
// ──────────────────────────────────────────────

export default function () {
  const roll = Math.random();

  let payload;
  let expectedFraud;

  if (roll < 0.60) {
    payload = legitTransaction();
    expectedFraud = false;
  } else if (roll < 0.90) {
    payload = fraudTransaction();
    expectedFraud = true;
  } else {
    payload = newCustomerTransaction();
    expectedFraud = false;
  }

  const res = http.post(
    `${BASE_URL}/fraud-score`,
    JSON.stringify(payload),
    { headers: { 'Content-Type': 'application/json' } },
  );

  const ok = check(res, {
    'status 200':           (r) => r.status === 200,
    'has approved field':   (r) => {
      try { return JSON.parse(r.body).approved !== undefined; } catch { return false; }
    },
    'has fraud_score field': (r) => {
      try { return typeof JSON.parse(r.body).fraud_score === 'number'; } catch { return false; }
    },
  });

  if (ok && res.status === 200) {
    let body;
    try { body = JSON.parse(res.body); } catch { return; }

    const approved = body.approved;
    if (expectedFraud) {
      if (!approved) {
        blockedFraud.add(1);
        detectionRate.add(1);
      } else {
        falseNegatives.add(1);
        detectionRate.add(0);
      }
    } else {
      if (approved) {
        approvedLegit.add(1);
      } else {
        falsePositives.add(1);
      }
    }
  }
}
