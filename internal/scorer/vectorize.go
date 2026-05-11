package scorer

import (
	"math"
	"time"

	"github.com/IgorBrizack/rinha-de-backend-2026/internal/domain"
)

// Normalization constants per REGRAS_DE_DETECCAO.md
const (
	maxAmount      = 10_000.0
	maxInstallment = 12.0
	maxRatio       = 10.0
	maxMinutes     = 1_440.0
	maxKm          = 1_000.0
	maxTxCount     = 20.0
	maxMerchantAvg = 10_000.0
)

// Vectorize converts a FraudInput into the 14-dimensional normalized vector
// used for KNN comparison. Sentinel value -1.0 indicates missing data.
func Vectorize(input domain.FraudInput, mccRisk map[string]float64) [14]float64 {
	tx := input.Transaction
	cust := input.Customer
	merch := input.Merchant
	term := input.Terminal

	var v [14]float64

	// 0: amount / max_amount
	v[0] = clamp(tx.Amount / maxAmount)

	// 1: installments / max_installments
	v[1] = clamp(float64(tx.Installments) / maxInstallment)

	// 2: (amount / customer_avg_amount) / max_ratio
	if cust.AvgAmount > 0 {
		v[2] = clamp((tx.Amount / cust.AvgAmount) / maxRatio)
	} else {
		v[2] = 0
	}

	// 3: hour_of_day / 23
	v[3] = float64(tx.RequestedAt.Hour()) / 23.0

	// 4: weekday / 6  (Monday=0 ... Sunday=6)
	wd := tx.RequestedAt.Weekday()
	if wd == time.Sunday {
		v[4] = 1.0
	} else {
		v[4] = float64(wd-1) / 6.0
	}

	// 5: minutes_since_last / max_minutes  (-1 if no prior tx)
	// 6: km_from_last / max_km             (-1 if no prior tx)
	if input.LastTransaction != nil {
		elapsed := tx.RequestedAt.Sub(input.LastTransaction.Timestamp).Minutes()
		if elapsed < 0 {
			elapsed = 0
		}
		v[5] = clamp(elapsed / maxMinutes)
		v[6] = clamp(input.LastTransaction.KmFromCurrent / maxKm)
	} else {
		v[5] = -1.0
		v[6] = -1.0
	}

	// 7: km_from_home / max_km
	v[7] = clamp(term.KmFromHome / maxKm)

	// 8: tx_count_24h / max_tx_count
	v[8] = clamp(float64(cust.TxCount24h) / maxTxCount)

	// 9: is_online
	v[9] = boolF(term.IsOnline)

	// 10: card_present
	v[10] = boolF(term.CardPresent)

	// 11: unknown merchant (not in customer's known_merchants list)
	v[11] = boolF(!contains(cust.KnownMerchants, merch.ID))

	// 12: mcc_risk lookup (default 0.5)
	if risk, ok := mccRisk[merch.MCC]; ok {
		v[12] = risk
	} else {
		v[12] = 0.5
	}

	// 13: merchant_avg_amount / max_amount
	v[13] = clamp(merch.AvgAmount / maxMerchantAvg)

	return v
}

func clamp(v float64) float64 {
	return math.Max(0, math.Min(1, v))
}

func boolF(b bool) float64 {
	if b {
		return 1.0
	}
	return 0.0
}

func contains(slice []string, target string) bool {
	for _, s := range slice {
		if s == target {
			return true
		}
	}
	return false
}
