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
	"os"
)

type record struct {
	Vector [14]float64 `json:"vector"`
	Label  string      `json:"label"`
}

func main() {
	input := flag.String("input", "", "path to references.json.gz")
	output := flag.String("output", "", "path to output binary file")
	flag.Parse()

	if *input == "" || *output == "" {
		log.Fatal("usage: preprocess -input <file.json.gz> -output <file.bin>")
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
	// Put the byte back so the decoder sees the full stream.
	if err := br.UnreadByte(); err != nil {
		log.Fatalf("unread byte: %v", err)
	}

	dec := json.NewDecoder(br)
	if isArray {
		// Consume the opening '['.
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

	out, err := os.Create(*output)
	if err != nil {
		log.Fatalf("create output: %v", err)
	}
	defer out.Close()

	bw := bufio.NewWriterSize(out, 8*1024*1024)

	// Binary layout: [count uint32 LE][dims uint32 LE][vectors flat i16][labels bytes]
	_ = binary.Write(bw, binary.LittleEndian, count)
	_ = binary.Write(bw, binary.LittleEndian, uint32(14))
	_ = binary.Write(bw, binary.LittleEndian, vectors)
	_, _ = bw.Write(labels)

	if err := bw.Flush(); err != nil {
		log.Fatalf("flush: %v", err)
	}

	log.Printf("done: %d records → %s", count, *output)
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
