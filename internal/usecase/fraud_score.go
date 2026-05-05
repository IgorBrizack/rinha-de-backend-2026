package usecase

import "github.com/IgorBrizack/rinha-de-backend-2026/internal/domain"

type ScoreFraud struct {
	scorer domain.FraudScorer
}

func NewScoreFraud(scorer domain.FraudScorer) *ScoreFraud {
	return &ScoreFraud{scorer: scorer}
}

func (uc *ScoreFraud) Execute(input domain.FraudInput) domain.FraudResult {
	return uc.scorer.Score(input)
}
