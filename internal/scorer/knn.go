package scorer

import (
	"encoding/binary"
	"encoding/json"
	"fmt"
	"math"
	"os"
	"runtime"
	"sync"

	"github.com/IgorBrizack/rinha-de-backend-2026/internal/domain"
)

const (
	kNeighbors     = 5
	fraudThreshold = 0.6
	nProbe         = 3 // IVF: clusters to probe per query (K=1024)
)

type KNN struct {
	vectors []int16
	labels  []uint8
	count   int
	mccRisk map[string]float64

	// IVF index (populated when binary was built with -ivf-k > 0)
	centroids      []int16
	clusterOffsets []uint32
	ivfK           int

	// Flat brute-force concurrency (only used when ivfK == 0)
	sem      chan struct{}
	nWorkers int
}

// NewKNN loads the binary reference dataset and the MCC-risk map.
//
// Supports two binary layouts:
//
//	v1 (legacy):  [count u32][dims u32][vectors flat i16][labels bytes]
//	v2 (IVF):     [count u32][dims u32][K u32][K×14 centroids i16][(K+1) offsets u32][vectors sorted by cluster][labels]
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

	// Detect v1 vs v2 by expected file size.
	v1Size := 8 + count*dims*2 + count
	K := 0
	offset := 8

	if len(data) != v1Size {
		if len(data) < 12 {
			return nil, fmt.Errorf("dataset truncated (header)")
		}
		K = int(binary.LittleEndian.Uint32(data[8:12]))
		offset = 12
	}

	var centroids []int16
	var clusterOffsets []uint32

	if K > 0 {
		centBytes := K * dims * 2
		if len(data) < offset+centBytes {
			return nil, fmt.Errorf("dataset truncated (centroids)")
		}
		centroids = make([]int16, K*dims)
		for i := range centroids {
			centroids[i] = int16(binary.LittleEndian.Uint16(data[offset+i*2:]))
		}
		offset += centBytes

		offBytes := (K + 1) * 4
		if len(data) < offset+offBytes {
			return nil, fmt.Errorf("dataset truncated (offsets)")
		}
		clusterOffsets = make([]uint32, K+1)
		for i := range clusterOffsets {
			clusterOffsets[i] = binary.LittleEndian.Uint32(data[offset+i*4:])
		}
		offset += offBytes
	}

	vectorBytes := count * dims * 2
	labelOffset := offset + vectorBytes
	if len(data) < labelOffset+count {
		return nil, fmt.Errorf("dataset truncated (vectors/labels)")
	}

	raw := data[offset : offset+vectorBytes]
	vectors := make([]int16, count*dims)
	for i := range vectors {
		vectors[i] = int16(binary.LittleEndian.Uint16(raw[i*2:]))
	}
	labels := data[labelOffset : labelOffset+count]

	mccRisk, err := loadMCCRisk(mccRiskPath)
	if err != nil {
		return nil, fmt.Errorf("load mcc_risk: %w", err)
	}

	knn := &KNN{
		vectors:        vectors,
		labels:         labels,
		count:          count,
		mccRisk:        mccRisk,
		centroids:      centroids,
		clusterOffsets: clusterOffsets,
		ivfK:           K,
	}

	if K == 0 {
		knn.nWorkers = runtime.GOMAXPROCS(0)
		sem := make(chan struct{}, 1)
		sem <- struct{}{}
		knn.sem = sem
	}

	return knn, nil
}

type neighbor struct {
	dist  int64
	label uint8
}

type centDist struct {
	dist int64
	idx  int
}

var centDistPool = sync.Pool{
	New: func() any {
		s := make([]centDist, 1024)
		return &s
	},
}

func (k *KNN) Count() int { return k.count }

func (k *KNN) Score(input domain.FraudInput) domain.FraudResult {
	query := Vectorize(input, k.mccRisk)
	qi16 := quantizeVec(query)

	var heap [kNeighbors]neighbor
	var filled int

	if k.ivfK > 0 {
		heap, filled = k.ivfSearch(qi16)
	} else {
		heap, filled = k.flatSearch(qi16)
	}

	fraudCount := 0
	for i := 0; i < filled; i++ {
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

// ivfSearch finds k-nearest neighbours using the precomputed IVF index.
// Scans only nProbe clusters instead of the full dataset.
func (k *KNN) ivfSearch(qi16 [14]int16) ([kNeighbors]neighbor, int) {
	cdPtr := centDistPool.Get().(*[]centDist)
	cd := (*cdPtr)[:k.ivfK]
	defer centDistPool.Put(cdPtr)

	for c := 0; c < k.ivfK; c++ {
		cd[c] = centDist{sqDist(qi16[:], k.centroids[c*14:c*14+14]), c}
	}

	// Partial selection sort: move top nProbe to the front (O(K × nProbe)).
	np := min(nProbe, k.ivfK)
	for p := 0; p < np; p++ {
		best := p
		for i := p + 1; i < k.ivfK; i++ {
			if cd[i].dist < cd[best].dist {
				best = i
			}
		}
		cd[p], cd[best] = cd[best], cd[p]
	}

	heap := [kNeighbors]neighbor{}
	heapFull := false
	worstIdx := 0
	worstDist := int64(math.MaxInt64)
	filled := 0

	for p := 0; p < np; p++ {
		c := cd[p].idx
		start := int(k.clusterOffsets[c])
		end := int(k.clusterOffsets[c+1])
		for i := start; i < end; i++ {
			d := sqDist(qi16[:], k.vectors[i*14:i*14+14])
			if !heapFull {
				heap[filled] = neighbor{d, k.labels[i]}
				filled++
				if filled == kNeighbors {
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
	}
	return heap, filled
}

// flatSearch is the legacy parallel brute-force path (used when ivfK == 0).
func (k *KNN) flatSearch(qi16 [14]int16) ([kNeighbors]neighbor, int) {
	<-k.sem
	defer func() { k.sem <- struct{}{} }()

	type chunkResult struct {
		heap  [kNeighbors]neighbor
		count int
	}

	chunkSize := (k.count + k.nWorkers - 1) / k.nWorkers
	results := make([]chunkResult, k.nWorkers)

	var wg sync.WaitGroup
	for w := 0; w < k.nWorkers; w++ {
		start := w * chunkSize
		if start >= k.count {
			break
		}
		end := min(start+chunkSize, k.count)
		wg.Add(1)
		go func(w, start, end int) {
			defer wg.Done()
			results[w].heap, results[w].count = k.searchRange(qi16, start, end)
		}(w, start, end)
	}
	wg.Wait()

	final := [kNeighbors]neighbor{}
	finalFull := false
	worstIdx := 0
	worstDist := int64(math.MaxInt64)
	filled := 0

	for _, r := range results {
		for i := 0; i < r.count; i++ {
			nb := r.heap[i]
			if !finalFull {
				final[filled] = nb
				filled++
				if filled == kNeighbors {
					finalFull = true
					worstIdx, worstDist = heapWorst(final[:])
				}
				continue
			}
			if nb.dist < worstDist {
				final[worstIdx] = nb
				worstIdx, worstDist = heapWorst(final[:])
			}
		}
	}
	return final, filled
}

// searchRange scans vectors[start:end] and returns the local k-nearest heap.
func (k *KNN) searchRange(qi16 [14]int16, start, end int) ([kNeighbors]neighbor, int) {
	heap := [kNeighbors]neighbor{}
	heapFull := false
	worstIdx := 0
	worstDist := int64(math.MaxInt64)
	filled := 0

	for i := start; i < end; i++ {
		ref := k.vectors[i*14 : i*14+14]
		d := sqDist(qi16[:], ref)

		if !heapFull {
			heap[filled] = neighbor{d, k.labels[i]}
			filled++
			if filled == kNeighbors {
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

	return heap, filled
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
