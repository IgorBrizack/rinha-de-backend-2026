package scorer

import "github.com/IgorBrizack/rinha-de-backend-2026/internal/domain"

// NoOp is a placeholder scorer that approves all transactions.
// Replace with real fraud detection logic when requirements are defined.
type NoOp struct{}

func (s *NoOp) Score(_ domain.FraudInput) domain.FraudResult {
	return domain.FraudResult{
		Approved:   true,
		FraudScore: 0.0,
	}
}
