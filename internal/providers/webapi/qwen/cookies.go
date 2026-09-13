package qwenweb

import (
	"fmt"
	mrand "math/rand/v2"
	"strings"
	"time"
)

// customB64Chars is the alphabet used by the ssxmod cookie encoder.
const customB64Chars = "DGi0YA7BemWnQjCl4_bR3f8SKIF9tUz/xhr2oEOgPpac=61ZqwTudLkM5vHyNXsVJ"

// bitWriter packs 6-bit codes MSB-first, as the reference encoder does.
type bitWriter struct {
	bits     int
	value    int
	position int
	out      []byte
}

func (b *bitWriter) emit() {
	b.out = append(b.out, customB64Chars[b.value])
	b.value = 0
	b.position = 0
}

// writeCode writes n bits of code, least-significant bit first.
func (b *bitWriter) writeCode(code, n int) {
	for i := 0; i < n; i++ {
		b.value = (b.value << 1) | (code & 1)
		if b.position == b.bits-1 {
			b.emit()
		} else {
			b.position++
		}
		code >>= 1
	}
}

func (b *bitWriter) writeZeros(n int) {
	for i := 0; i < n; i++ {
		b.value <<= 1
		if b.position == b.bits-1 {
			b.emit()
		} else {
			b.position++
		}
	}
}

func (b *bitWriter) flush() {
	for {
		b.value <<= 1
		if b.position == b.bits-1 {
			b.emit()
			return
		}
		b.position++
	}
}

// lzwCompress ports qwen-reverse cookies.py lzw_compress (MIT).
func lzwCompress(data string, bits int, charFunc func(int) byte) string {
	dictionary := map[string]int{}
	dictToCreate := map[string]bool{}
	c, wc, w := "", "", ""
	enlargeIn := 2
	dictSize := 3
	numBits := 2
	var bw bitWriter
	bw.bits = bits
	bw.out = make([]byte, 0, len(data))

	for i := 0; i < len(data); i++ {
		c = string(data[i])
		if _, ok := dictionary[c]; !ok {
			dictionary[c] = dictSize
			dictSize++
			dictToCreate[c] = true
		}
		wc = w + c
		if _, ok := dictionary[wc]; ok {
			w = wc
			continue
		}
		if dictToCreate[w] {
			if int(w[0]) < 256 {
				bw.writeZeros(numBits)
				charCode := int(w[0])
				for j := 0; j < 8; j++ {
					bw.writeCode(charCode&1, 1)
					charCode >>= 1
				}
			} else {
				bw.writeCode(1, numBits)
				charCode := int(w[0])
				for j := 0; j < 16; j++ {
					bw.writeCode(charCode&1, 1)
					charCode >>= 1
				}
			}
			enlargeIn--
			if enlargeIn == 0 {
				enlargeIn = 1 << numBits
				numBits++
			}
			delete(dictToCreate, w)
		} else {
			charCode := dictionary[w]
			bw.writeCode(charCode, numBits)
		}
		// The reference decrements once more on the common path (twice total
		// on the literal path) — mirrored exactly so the encoding matches.
		enlargeIn--
		if enlargeIn == 0 {
			enlargeIn = 1 << numBits
			numBits++
		}
		dictionary[wc] = dictSize
		dictSize++
		w = c
	}

	if w != "" {
		if dictToCreate[w] {
			if int(w[0]) < 256 {
				bw.writeZeros(numBits)
				charCode := int(w[0])
				for j := 0; j < 8; j++ {
					bw.writeCode(charCode&1, 1)
					charCode >>= 1
				}
			} else {
				bw.writeCode(1, numBits)
				charCode := int(w[0])
				for j := 0; j < 16; j++ {
					bw.writeCode(charCode&1, 1)
					charCode >>= 1
				}
			}
			enlargeIn--
			if enlargeIn == 0 {
				enlargeIn = 1 << numBits
				numBits++
			}
			delete(dictToCreate, w)
		} else {
			charCode := dictionary[w]
			bw.writeCode(charCode, numBits)
		}
		enlargeIn--
		if enlargeIn == 0 {
			enlargeIn = 1 << numBits
			numBits++
		}
	}
	bw.writeCode(2, numBits)
	bw.flush()
	return string(bw.out)
}

func customEncode(data string, urlSafe bool) string {
	compressed := lzwCompress(data, 6, func(i int) byte { return customB64Chars[i] })
	if urlSafe {
		return compressed
	}
	switch len(compressed) % 4 {
	case 1:
		return compressed + "==="
	case 2:
		return compressed + "=="
	case 3:
		return compressed + "="
	}
	return compressed
}

// generateCookies ports qwen-reverse cookies.py generate_cookies (MIT).
func generateCookies(fp string) (string, string) {
	processed := strings.Split(fp, "^")
	if p := strings.Split(processed[16], "|"); len(p) == 2 {
		processed[16] = fmt.Sprintf("%s|%d", p[0], mrand.Uint32())
	}
	for _, idx := range []int{17, 18, 31, 34} {
		processed[idx] = fmt.Sprint(mrand.Uint32())
	}
	processed[36] = fmt.Sprint(10 + mrand.IntN(91))
	processed[33] = fmt.Sprint(time.Now().UnixMilli())

	itnaData := strings.Join(processed, "^")
	itna := "1-" + customEncode(itnaData, true)

	f := func(i int) string { return processed[i] }
	itna2Data := strings.Join([]string{
		f(0), f(1), f(23), "0", "", "0", "", "", "0", "0", "0",
		f(32), f(33), "0", "0", "0", "0", "0",
	}, "^")
	itna2 := "1-" + customEncode(itna2Data, true)
	return itna, itna2
}
