// Copyright 2011 The LevelDB-Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package table

import (
	"bufio"
	"bytes"
	"encoding/binary"
	"fmt"
	"io"
	"io/ioutil"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"github.com/golang/leveldb/bloom"
	"github.com/golang/leveldb/db"
	"github.com/golang/leveldb/memfs"
)

// nonsenseWords are words that aren't in ../testdata/h.txt.
var nonsenseWords = []string{
	// Edge cases.
	"",
	"\x00",
	"\xff",
	"`",
	"a\x00",
	"aaaaaa",
	"pol\x00nius",
	"youth\x00",
	"youti",
	"zzzzzz",
	// Capitalized versions of actual words in ../testdata/h.txt.
	"A",
	"Hamlet",
	"thEE",
	"YOUTH",
	// The following were generated by http://soybomb.com/tricks/words/
	"pectures",
	"exectly",
	"tricatrippian",
	"recens",
	"whiratroce",
	"troped",
	"balmous",
	"droppewry",
	"toilizing",
	"crocias",
	"eathrass",
	"cheakden",
	"speablett",
	"skirinies",
	"prefing",
	"bonufacision",
}

var (
	wordCount = map[string]string{}
	minWord   = ""
	maxWord   = ""
)

func init() {
	f, err := os.Open(filepath.FromSlash("../testdata/h.txt"))
	if err != nil {
		panic(err)
	}
	defer f.Close()
	r := bufio.NewReader(f)

	for first := true; ; {
		s, err := r.ReadBytes('\n')
		if err == io.EOF {
			break
		}
		if err != nil {
			panic(err)
		}
		k := strings.TrimSpace(string(s[8:]))
		v := strings.TrimSpace(string(s[:8]))
		wordCount[k] = v

		if first {
			first = false
			minWord = k
			maxWord = k
			continue
		}
		if minWord > k {
			minWord = k
		}
		if maxWord < k {
			maxWord = k
		}
	}

	if len(wordCount) != 1710 {
		panic(fmt.Sprintf("h.txt entry count: got %d, want %d", len(wordCount), 1710))
	}

	for _, s := range nonsenseWords {
		if _, ok := wordCount[s]; ok {
			panic(fmt.Sprintf("nonsense word %q was in h.txt", s))
		}
	}
}

func check(f db.File, fp db.FilterPolicy) error {
	r := NewReader(f, &db.Options{
		FilterPolicy:    fp,
		VerifyChecksums: true,
	})
	// Check that each key/value pair in wordCount is also in the table.
	for k, v := range wordCount {
		// Check using Get.
		if v1, err := r.Get([]byte(k), nil); string(v1) != string(v) || err != nil {
			return fmt.Errorf("Get %q: got (%q, %v), want (%q, %v)", k, v1, err, v, error(nil))
		} else if len(v1) != cap(v1) {
			return fmt.Errorf("Get %q: len(v1)=%d, cap(v1)=%d", k, len(v1), cap(v1))
		}

		// Check using Find.
		i := r.Find([]byte(k), nil)
		if !i.Next() || string(i.Key()) != k {
			return fmt.Errorf("Find %q: key was not in the table", k)
		}
		if k1 := i.Key(); len(k1) != cap(k1) {
			return fmt.Errorf("Find %q: len(k1)=%d, cap(k1)=%d", k, len(k1), cap(k1))
		}
		if string(i.Value()) != v {
			return fmt.Errorf("Find %q: got value %q, want %q", k, i.Value(), v)
		}
		if v1 := i.Value(); len(v1) != cap(v1) {
			return fmt.Errorf("Find %q: len(v1)=%d, cap(v1)=%d", k, len(v1), cap(v1))
		}
		if err := i.Close(); err != nil {
			return err
		}
	}

	// Check that nonsense words are not in the table.
	for _, s := range nonsenseWords {
		// Check using Get.
		if _, err := r.Get([]byte(s), nil); err != db.ErrNotFound {
			return fmt.Errorf("Get %q: got %v, want ErrNotFound", s, err)
		}

		// Check using Find.
		i := r.Find([]byte(s), nil)
		if i.Next() && s == string(i.Key()) {
			return fmt.Errorf("Find %q: unexpectedly found key in the table", s)
		}
		if err := i.Close(); err != nil {
			return err
		}
	}

	// Check that the number of keys >= a given start key matches the expected number.
	var countTests = []struct {
		count int
		start string
	}{
		// cat h.txt | cut -c 9- | wc -l gives 1710.
		{1710, ""},
		// cat h.txt | cut -c 9- | grep -v "^[a-b]" | wc -l gives 1522.
		{1522, "c"},
		// cat h.txt | cut -c 9- | grep -v "^[a-j]" | wc -l gives 940.
		{940, "k"},
		// cat h.txt | cut -c 9- | grep -v "^[a-x]" | wc -l gives 12.
		{12, "y"},
		// cat h.txt | cut -c 9- | grep -v "^[a-z]" | wc -l gives 0.
		{0, "~"},
	}
	for _, ct := range countTests {
		n, i := 0, r.Find([]byte(ct.start), nil)
		for i.Next() {
			n++
		}
		if err := i.Close(); err != nil {
			return err
		}
		if n != ct.count {
			return fmt.Errorf("count %q: got %d, want %d", ct.start, n, ct.count)
		}
	}

	return r.Close()
}

var (
	memFileSystem = memfs.New()
	tmpFileCount  int
)

func build(compression db.Compression) (db.File, error) {
	// Create a sorted list of wordCount's keys.
	keys := make([]string, len(wordCount))
	i := 0
	for k := range wordCount {
		keys[i] = k
		i++
	}
	sort.Strings(keys)

	// Write the key/value pairs to a new table, in increasing key order.
	filename := fmt.Sprintf("/tmp%d", tmpFileCount)
	f0, err := memFileSystem.Create(filename)
	if err != nil {
		return nil, err
	}
	defer f0.Close()
	tmpFileCount++
	w := NewWriter(f0, &db.Options{
		Compression: compression,
	})
	for _, k := range keys {
		v := wordCount[k]
		if err := w.Set([]byte(k), []byte(v), nil); err != nil {
			return nil, err
		}
	}
	if err := w.Close(); err != nil {
		return nil, err
	}

	// Re-open that filename for reading.
	f1, err := memFileSystem.Open(filename)
	if err != nil {
		return nil, err
	}
	return f1, nil
}

func testReader(t *testing.T, filename string, fp db.FilterPolicy) {
	// Check that we can read a pre-made table.
	f, err := os.Open(filepath.FromSlash("../testdata/" + filename))
	if err != nil {
		t.Error(err)
		return
	}
	err = check(f, fp)
	if err != nil {
		t.Error(err)
		return
	}
}

func TestReaderDefaultCompression(t *testing.T) { testReader(t, "h.ldb", nil) }
func TestReaderNoCompression(t *testing.T)      { testReader(t, "h.no-compression.ldb", nil) }
func TestReaderBloomIgnored(t *testing.T)       { testReader(t, "h.bloom.no-compression.ldb", nil) }

func TestReaderBloomUsed(t *testing.T) {
	// wantActualNegatives is the minimum number of nonsense words (i.e. false
	// positives or true negatives) to run through our filter. Some nonsense
	// words might be rejected even before the filtering step, if they are out
	// of the [minWord, maxWord] range of keys in the table.
	wantActualNegatives := 0
	for _, s := range nonsenseWords {
		if minWord < s && s < maxWord {
			wantActualNegatives++
		}
	}

	for _, degenerate := range []bool{false, true} {
		c := &countingFilterPolicy{
			FilterPolicy: bloom.FilterPolicy(10),
			degenerate:   degenerate,
		}
		testReader(t, "h.bloom.no-compression.ldb", c)

		if c.truePositives != len(wordCount) {
			t.Errorf("degenerate=%t: true positives: got %d, want %d", degenerate, c.truePositives, len(wordCount))
		}
		if c.falseNegatives != 0 {
			t.Errorf("degenerate=%t: false negatives: got %d, want %d", degenerate, c.falseNegatives, 0)
		}

		if got := c.falsePositives + c.trueNegatives; got < wantActualNegatives {
			t.Errorf("degenerate=%t: actual negatives (false positives + true negatives): "+
				"got %d (%d + %d), want >= %d",
				degenerate, got, c.falsePositives, c.trueNegatives, wantActualNegatives)
		}

		if !degenerate {
			// The true negative count should be much greater than the false
			// positive count.
			if c.trueNegatives < 10*c.falsePositives {
				t.Errorf("degenerate=%t: true negative to false positive ratio (%d:%d) is too small",
					degenerate, c.trueNegatives, c.falsePositives)
			}
		}
	}
}

func TestBloomFilterFalsePositiveRate(t *testing.T) {
	f, err := os.Open(filepath.FromSlash("../testdata/h.bloom.no-compression.ldb"))
	if err != nil {
		t.Fatal(err)
	}
	c := &countingFilterPolicy{
		FilterPolicy: bloom.FilterPolicy(1),
	}
	r := NewReader(f, &db.Options{
		FilterPolicy: c,
	})

	const n = 10000
	// key is a buffer that will be re-used for n Get calls, each with a
	// different key. The "m" in the 2-byte prefix means that the key falls in
	// the [minWord, maxWord] range and so will not be rejected prior to
	// applying the Bloom filter. The "!" in the 2-byte prefix means that the
	// key is not actually in the table. The filter will only see actual
	// negatives: false positives or true negatives.
	key := []byte("m!....")
	for i := 0; i < n; i++ {
		binary.LittleEndian.PutUint32(key[2:6], uint32(i))
		r.Get(key, nil)
	}

	if c.truePositives != 0 {
		t.Errorf("true positives: got %d, want 0", c.truePositives)
	}
	if c.falseNegatives != 0 {
		t.Errorf("false negatives: got %d, want 0", c.falseNegatives)
	}
	if got := c.falsePositives + c.trueNegatives; got != n {
		t.Errorf("actual negatives (false positives + true negatives): got %d (%d + %d), want %d",
			got, c.falsePositives, c.trueNegatives, n)
	}

	// According the the comments in the C++ LevelDB code, the false positive
	// rate should be approximately 1% for for bloom.FilterPolicy(10). The 10
	// was the parameter used to write the .ldb file. When reading the file,
	// the 1 in the bloom.FilterPolicy(1) above doesn't matter, only the
	// bloom.FilterPolicy matters.
	if got := float64(100*c.falsePositives) / n; got < 0.2 || 5 < got {
		t.Errorf("false positive rate: got %v%%, want approximately 1%%", got)
	}
}

type countingFilterPolicy struct {
	db.FilterPolicy
	degenerate bool

	truePositives  int
	falsePositives int
	falseNegatives int
	trueNegatives  int
}

func (c *countingFilterPolicy) MayContain(filter, key []byte) bool {
	got := true
	if c.degenerate {
		// When degenerate is true, we override the embedded FilterPolicy's
		// MayContain method to always return true. Doing so is a valid, if
		// inefficient, implementation of the FilterPolicy interface.
	} else {
		got = c.FilterPolicy.MayContain(filter, key)
	}
	_, want := wordCount[string(key)]

	switch {
	case got && want:
		c.truePositives++
	case got && !want:
		c.falsePositives++
	case !got && want:
		c.falseNegatives++
	case !got && !want:
		c.trueNegatives++
	}
	return got
}

func TestWriter(t *testing.T) {
	// Check that we can read a freshly made table.
	f, err := build(db.DefaultCompression)
	if err != nil {
		t.Fatal(err)
	}
	err = check(f, nil)
	if err != nil {
		t.Fatal(err)
	}
}

func TestNoCompressionOutput(t *testing.T) {
	// Check that a freshly made NoCompression table is byte-for-byte equal
	// to a pre-made table.
	a, err := ioutil.ReadFile(filepath.FromSlash("../testdata/h.no-compression.ldb"))
	if err != nil {
		t.Fatal(err)
	}
	f, err := build(db.NoCompression)
	if err != nil {
		t.Fatal(err)
	}
	stat, err := f.Stat()
	if err != nil {
		t.Fatal(err)
	}
	b := make([]byte, stat.Size())
	_, err = f.ReadAt(b, 0)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(a, b) {
		t.Fatal("built table does not match pre-made table")
	}
}

func TestBlockIter(t *testing.T) {
	// k is a block that maps three keys "apple", "apricot", "banana" to empty strings.
	k := block([]byte("\x00\x05\x00apple\x02\x05\x00ricot\x00\x06\x00banana\x00\x00\x00\x00\x01\x00\x00\x00"))
	var testcases = []struct {
		index int
		key   string
	}{
		{0, ""},
		{0, "a"},
		{0, "aaaaaaaaaaaaaaa"},
		{0, "app"},
		{0, "apple"},
		{1, "appliance"},
		{1, "apricos"},
		{1, "apricot"},
		{2, "azzzzzzzzzzzzzz"},
		{2, "b"},
		{2, "banan"},
		{2, "banana"},
		{3, "banana\x00"},
		{3, "c"},
	}
	for _, tc := range testcases {
		i, err := k.seek(db.DefaultComparer, []byte(tc.key))
		if err != nil {
			t.Fatal(err)
		}
		for j, kWant := range []string{"apple", "apricot", "banana"}[tc.index:] {
			if !i.Next() {
				t.Fatalf("key=%q, index=%d, j=%d: Next got false, want true", tc.key, tc.index, j)
			}
			if kGot := string(i.Key()); kGot != kWant {
				t.Fatalf("key=%q, index=%d, j=%d: got %q, want %q", tc.key, tc.index, j, kGot, kWant)
			}
		}
		if i.Next() {
			t.Fatalf("key=%q, index=%d: Next got true, want false", tc.key, tc.index)
		}
		if err := i.Close(); err != nil {
			t.Fatalf("key=%q, index=%d: got err=%v", tc.key, tc.index, err)
		}
	}
}

func TestFinalBlockIsWritten(t *testing.T) {
	const blockSize = 100
	keys := []string{"A", "B", "C", "D", "E", "F", "G", "H", "I", "J"}
	valueLengths := []int{0, 1, 22, 28, 33, 40, 50, 61, 87, 100, 143, 200}
	xxx := bytes.Repeat([]byte("x"), valueLengths[len(valueLengths)-1])

	for nk := 0; nk <= len(keys); nk++ {
	loop:
		for _, vLen := range valueLengths {
			got, memFS := 0, memfs.New()

			wf, err := memFS.Create("foo")
			if err != nil {
				t.Errorf("nk=%d, vLen=%d: memFS create: %v", nk, vLen, err)
				continue
			}
			w := NewWriter(wf, &db.Options{
				BlockSize: blockSize,
			})
			for _, k := range keys[:nk] {
				if err := w.Set([]byte(k), xxx[:vLen], nil); err != nil {
					t.Errorf("nk=%d, vLen=%d: set: %v", nk, vLen, err)
					continue loop
				}
			}
			if err := w.Close(); err != nil {
				t.Errorf("nk=%d, vLen=%d: writer close: %v", nk, vLen, err)
				continue
			}

			rf, err := memFS.Open("foo")
			if err != nil {
				t.Errorf("nk=%d, vLen=%d: memFS open: %v", nk, vLen, err)
				continue
			}
			r := NewReader(rf, nil)
			i := r.Find(nil, nil)
			for i.Next() {
				got++
			}
			if err := i.Close(); err != nil {
				t.Errorf("nk=%d, vLen=%d: Iterator close: %v", nk, vLen, err)
				continue
			}
			if err := r.Close(); err != nil {
				t.Errorf("nk=%d, vLen=%d: reader close: %v", nk, vLen, err)
				continue
			}

			if got != nk {
				t.Errorf("nk=%2d, vLen=%3d: got %2d keys, want %2d", nk, vLen, got, nk)
				continue
			}
		}
	}
}
