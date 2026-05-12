package main

import (
	"bufio"
	"compress/gzip"
	"encoding/binary"
	"encoding/json"
	"flag"
	"io"
	"log"
	"math"
	"math/rand"
	"os"
	"runtime"
	"sort"
	"sync"
)

type record struct {
	Vector [14]float64 `json:"vector"`
	Label  string      `json:"label"`
}

func main() {
	input      := flag.String("input", "", "path to references.json.gz")
	output     := flag.String("output", "", "path to output binary file")
	maxSamples := flag.Int("max-samples", 0, "random sample size (0 = keep all)")
	ivfK       := flag.Int("ivf-k", 0, "number of IVF centroids (0 = no IVF)")
	ivfIters   := flag.Int("ivf-iters", 25, "k-means iterations")
	flag.Parse()

	if *input == "" || *output == "" {
		log.Fatal("usage: preprocess -input <file.json.gz> -output <file.bin> [-max-samples N] [-ivf-k K]")
	}

	f, err := os.Open(*input)
	if err != nil {
		log.Fatalf("open input: %v", err)
	}
	defer f.Close()

	gz, err := gzip.NewReader(f)
	if err != nil {
		log.Fatalf("gzip reader: %v", err)
	}
	defer gz.Close()

	br := bufio.NewReaderSize(gz, 8*1024*1024)

	// Peek first non-whitespace byte to detect JSON array vs NDJSON.
	firstByte, err := br.ReadByte()
	if err != nil {
		log.Fatalf("read first byte: %v", err)
	}
	isArray := firstByte == '['
	if err := br.UnreadByte(); err != nil {
		log.Fatalf("unread byte: %v", err)
	}

	dec := json.NewDecoder(br)
	if isArray {
		if _, err := dec.Token(); err != nil {
			log.Fatalf("read array open: %v", err)
		}
	}

	// Pre-allocate for ~3.1M records to avoid reallocations.
	const approxCount = 3_100_000
	vectors := make([]int16, 0, approxCount*14)
	labels := make([]uint8, 0, approxCount)

	count := uint32(0)
	for {
		if isArray && !dec.More() {
			break
		}
		var r record
		if err := dec.Decode(&r); err != nil {
			if err == io.EOF {
				break
			}
			log.Fatalf("decode record %d: %v", count+1, err)
		}
		for _, v := range r.Vector {
			vectors = append(vectors, quantize(v))
		}
		if r.Label == "fraud" {
			labels = append(labels, 1)
		} else {
			labels = append(labels, 0)
		}
		count++
		if count%500_000 == 0 {
			log.Printf("processed %d records...", count)
		}
	}

	// Optionally subsample while preserving class distribution.
	if *maxSamples > 0 && count > uint32(*maxSamples) {
		originalCount := count
		rng := rand.New(rand.NewSource(42))
		indices := make([]int, int(count))
		for i := range indices {
			indices[i] = i
		}
		rng.Shuffle(len(indices), func(i, j int) {
			indices[i], indices[j] = indices[j], indices[i]
		})
		indices = indices[:*maxSamples]
		sort.Ints(indices)

		newVectors := make([]int16, *maxSamples*14)
		newLabels := make([]uint8, *maxSamples)
		for i, idx := range indices {
			copy(newVectors[i*14:], vectors[idx*14:idx*14+14])
			newLabels[i] = labels[idx]
		}
		vectors = newVectors
		labels = newLabels
		count = uint32(*maxSamples)
		log.Printf("sampled %d records from %d", count, originalCount)
	}

	// Optionally build IVF index.
	K := uint32(0)
	var centroids []int16
	var clusterOffsets []uint32

	if *ivfK > 0 {
		log.Printf("building IVF index: K=%d, iters=%d, N=%d", *ivfK, *ivfIters, count)
		rng := rand.New(rand.NewSource(42))
		var assignments []int32
		centroids, assignments = kmeanspp(vectors, int(count), *ivfK, *ivfIters, rng)
		K = uint32(*ivfK)

		// Compute cluster sizes and offsets.
		clusterOffsets = make([]uint32, K+1)
		for _, c := range assignments {
			clusterOffsets[c+1]++
		}
		for c := uint32(1); c <= K; c++ {
			clusterOffsets[c] += clusterOffsets[c-1]
		}

		// Sort vectors and labels by cluster ID.
		pos := make([]int, K)
		for c := uint32(0); c < K; c++ {
			pos[c] = int(clusterOffsets[c])
		}
		sortedIdx := make([]int, count)
		for i, c := range assignments {
			sortedIdx[pos[c]] = i
			pos[c]++
		}
		newVectors := make([]int16, int(count)*14)
		newLabels := make([]uint8, count)
		for j, i := range sortedIdx {
			copy(newVectors[j*14:], vectors[i*14:i*14+14])
			newLabels[j] = labels[i]
		}
		vectors = newVectors
		labels = newLabels
		log.Printf("IVF index built: %d clusters", K)
	}

	out, err := os.Create(*output)
	if err != nil {
		log.Fatalf("create output: %v", err)
	}
	defer out.Close()

	bw := bufio.NewWriterSize(out, 8*1024*1024)

	// Binary layout v2:
	// [count uint32][dims uint32][K uint32]
	// If K>0: [K×14 centroids int16][(K+1) offsets uint32]
	// [count×14 vectors int16 sorted by cluster if K>0]
	// [count labels bytes]
	_ = binary.Write(bw, binary.LittleEndian, count)
	_ = binary.Write(bw, binary.LittleEndian, uint32(14))
	_ = binary.Write(bw, binary.LittleEndian, K)
	if K > 0 {
		_ = binary.Write(bw, binary.LittleEndian, centroids)
		_ = binary.Write(bw, binary.LittleEndian, clusterOffsets)
	}
	_ = binary.Write(bw, binary.LittleEndian, vectors)
	_, _ = bw.Write(labels)

	if err := bw.Flush(); err != nil {
		log.Fatalf("flush: %v", err)
	}

	log.Printf("done: %d records, K=%d → %s", count, K, *output)
}

// quantize maps [0.0, 1.0] → [0, 32767] and negative values → MinInt16 (sentinel for missing data).
func quantize(v float64) int16 {
	if v < 0 {
		return math.MinInt16
	}
	if v >= 1.0 {
		return 32767
	}
	return int16(v * 32767)
}

// sqDistSlice computes the sentinel-aware squared Euclidean distance between two i16 slices.
func sqDistSlice(a, b []int16) int64 {
	const sentinelPenalty = int64(16383 * 16383)
	var sum int64
	for i := range a {
		sa := a[i] == math.MinInt16
		sb := b[i] == math.MinInt16
		if sa && sb {
			continue
		}
		if sa != sb {
			sum += sentinelPenalty
			continue
		}
		d := int64(a[i]) - int64(b[i])
		sum += d * d
	}
	return sum
}

// kmeanspp clusters vecs (shape count×14) into K groups using k-means++ seeding
// followed by Lloyd's iterations. Returns (centroids K×14, assignments per vector).
func kmeanspp(vecs []int16, count, K, iters int, rng *rand.Rand) ([]int16, []int32) {
	const dims = 14
	centroids := make([]int16, K*dims)
	assignments := make([]int32, count)

	// --- k-means++ seeding ---
	// Keep running minimum distances to already-chosen centroids.
	minDists := make([]int64, count)
	for i := range minDists {
		minDists[i] = math.MaxInt64
	}

	first := rng.Intn(count)
	copy(centroids[:dims], vecs[first*dims:])

	for c := 0; c < K; c++ {
		// Update minDists against centroid c.
		for i := 0; i < count; i++ {
			d := sqDistSlice(vecs[i*dims:i*dims+dims], centroids[c*dims:c*dims+dims])
			if d < minDists[i] {
				minDists[i] = d
			}
		}
		if c+1 >= K {
			break
		}
		// Sample next centroid proportional to minDist².
		total := float64(0)
		for _, d := range minDists {
			total += float64(d)
		}
		var chosen int
		if total == 0 {
			chosen = rng.Intn(count)
		} else {
			r := rng.Float64() * total
			cum := float64(0)
			chosen = count - 1
			for i := 0; i < count; i++ {
				cum += float64(minDists[i])
				if cum >= r {
					chosen = i
					break
				}
			}
		}
		copy(centroids[(c+1)*dims:], vecs[chosen*dims:])
	}

	// --- Lloyd's iterations ---
	nCPU := runtime.GOMAXPROCS(0)
	chunkSize := (count + nCPU - 1) / nCPU

	for iter := 0; iter < iters; iter++ {
		// Parallel assignment step.
		changed := make([]int32, nCPU)
		var wg sync.WaitGroup
		for w := 0; w < nCPU; w++ {
			start := w * chunkSize
			if start >= count {
				break
			}
			end := min(start+chunkSize, count)
			wg.Add(1)
			go func(w, start, end int) {
				defer wg.Done()
				for i := start; i < end; i++ {
					best := int32(0)
					bestD := int64(math.MaxInt64)
					for c := 0; c < K; c++ {
						d := sqDistSlice(vecs[i*dims:i*dims+dims], centroids[c*dims:c*dims+dims])
						if d < bestD {
							bestD = d
							best = int32(c)
						}
					}
					if assignments[i] != best {
						changed[w]++
						assignments[i] = best
					}
				}
			}(w, start, end)
		}
		wg.Wait()

		total := int32(0)
		for _, c := range changed {
			total += c
		}
		log.Printf("iter %d/%d: %d reassignments", iter+1, iters, total)
		if total == 0 {
			break
		}

		// Centroid update step.
		sums := make([]int64, K*dims)
		normalCounts := make([]int32, K*dims)
		clusterSizes := make([]int32, K)

		for i := 0; i < count; i++ {
			c := int(assignments[i])
			clusterSizes[c]++
			for d := 0; d < dims; d++ {
				v := vecs[i*dims+d]
				if v != math.MinInt16 {
					sums[c*dims+d] += int64(v)
					normalCounts[c*dims+d]++
				}
			}
		}

		for c := 0; c < K; c++ {
			sz := clusterSizes[c]
			for d := 0; d < dims; d++ {
				nc := normalCounts[c*dims+d]
				if nc > 0 && nc >= sz-nc {
					centroids[c*dims+d] = int16(sums[c*dims+d] / int64(nc))
				} else {
					centroids[c*dims+d] = math.MinInt16
				}
			}
		}
	}

	return centroids, assignments
}
