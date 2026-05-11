package scorer

import (
	"encoding/binary"
	"encoding/json"
	"fmt"
	"math"
	"os"

	"github.com/IgorBrizack/rinha-de-backend-2026/internal/domain"
)

const (
	kNeighbors     = 5
	fraudThreshold = 0.6
)

// KNN implements fraud scoring using k-nearest neighbours (k=5) over a
// precomputed i16-quantised reference dataset.
type KNN struct {
	vectors []int16
	labels  []uint8
	count   int
	mccRisk map[string]float64
}

// NewKNN loads the binary reference dataset and the MCC-risk map.
// Binary layout: [count uint32 LE][dims uint32 LE][vectors flat i16][labels bytes]
func NewKNN(dataPath, mccRiskPath string) (*KNN, error) {
	data, err := os.ReadFile(dataPath)
	if err != nil {
		return nil, fmt.Errorf("read dataset: %w", err)
	}
	if len(data) < 8 {
		return nil, fmt.Errorf("dataset too small")
	}

	count := int(binary.LittleEndian.Uint32(data[0:4]))
	dims := int(binary.LittleEndian.Uint32(data[4:8]))
	if dims != 14 {
		return nil, fmt.Errorf("expected 14 dims, got %d", dims)
	}

	vectorBytes := count * dims * 2
	labelOffset := 8 + vectorBytes
	if len(data) < labelOffset+count {
		return nil, fmt.Errorf("dataset truncated")
	}

	raw := data[8 : 8+vectorBytes]
	vectors := make([]int16, count*dims)
	for i := range vectors {
		vectors[i] = int16(binary.LittleEndian.Uint16(raw[i*2:]))
	}
	labels := data[labelOffset : labelOffset+count]

	mccRisk, err := loadMCCRisk(mccRiskPath)
	if err != nil {
		return nil, fmt.Errorf("load mcc_risk: %w", err)
	}

	return &KNN{
		vectors: vectors,
		labels:  labels,
		count:   count,
		mccRisk: mccRisk,
	}, nil
}

type neighbor struct {
	dist  int64
	label uint8
}

func (k *KNN) Count() int { return k.count }

func (k *KNN) Score(input domain.FraudInput) domain.FraudResult {
	query := Vectorize(input, k.mccRisk)
	qi16 := quantizeVec(query)

	// Maintain the k nearest neighbours using a bounded max-heap (worst-out policy).
	heap := [kNeighbors]neighbor{}
	worstIdx := 0
	heapFull := false
	worstDist := int64(math.MaxInt64)

	for i := 0; i < k.count; i++ {
		ref := k.vectors[i*14 : i*14+14]
		d := sqDist(qi16[:], ref)

		if !heapFull {
			heap[i] = neighbor{d, k.labels[i]}
			if i == kNeighbors-1 {
				heapFull = true
				worstIdx, worstDist = heapWorst(heap[:])
			}
			continue
		}

		if d < worstDist {
			heap[worstIdx] = neighbor{d, k.labels[i]}
			worstIdx, worstDist = heapWorst(heap[:])
		}
	}

	limit := kNeighbors
	if k.count < kNeighbors {
		limit = k.count
	}
	fraudCount := 0
	for i := 0; i < limit; i++ {
		if heap[i].label == 1 {
			fraudCount++
		}
	}

	fraudScore := float64(fraudCount) / float64(kNeighbors)
	return domain.FraudResult{
		Approved:   fraudScore < fraudThreshold,
		FraudScore: fraudScore,
	}
}

// heapWorst returns the index and distance of the farthest neighbour in the heap.
func heapWorst(heap []neighbor) (int, int64) {
	idx, d := 0, heap[0].dist
	for i := 1; i < len(heap); i++ {
		if heap[i].dist > d {
			idx, d = i, heap[i].dist
		}
	}
	return idx, d
}

// sqDist computes the squared euclidean distance between two i16 vectors.
// Sentinel MinInt16 on both sides → zero contribution; mixed → fixed penalty.
func sqDist(a, b []int16) int64 {
	const sentinelPenalty = int64(16383 * 16383)
	var sum int64
	for i := range a {
		isSentA := a[i] == math.MinInt16
		isSentB := b[i] == math.MinInt16
		if isSentA && isSentB {
			continue
		}
		if isSentA != isSentB {
			sum += sentinelPenalty
			continue
		}
		d := int64(a[i]) - int64(b[i])
		sum += d * d
	}
	return sum
}

// quantizeVec converts a float64 vector to i16.
func quantizeVec(v [14]float64) [14]int16 {
	var out [14]int16
	for i, f := range v {
		out[i] = quantize(f)
	}
	return out
}

func quantize(v float64) int16 {
	if v < 0 {
		return math.MinInt16
	}
	if v >= 1.0 {
		return 32767
	}
	return int16(v * 32767)
}

func loadMCCRisk(path string) (map[string]float64, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var m map[string]float64
	if err := json.Unmarshal(data, &m); err != nil {
		return nil, err
	}
	return m, nil
}
