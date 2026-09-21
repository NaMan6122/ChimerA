package deepseekweb

import (
	"encoding/binary"
	"math/bits"
)

// DeepSeekHashV1 is a deliberately non-standard hash, and this file exists
// because no standard library provides it:
//
//   - it is Keccak-f[1600] with SHA-3 style 0x06 domain padding, NOT the legacy
//     Keccak 0x01 padding that x/crypto/sha3.NewLegacyKeccak256 uses; and
//   - it runs only 23 rounds, using round constants RC[1]..RC[23] — round 0 is
//     skipped — whereas both SHA3-256 and Keccak-256 run 24.
//
// Either deviation alone is enough to produce a different digest, so
// crypto/sha3 (FIPS, 24 rounds) and x/crypto (legacy padding, 24 rounds) both
// fail to reproduce it. The construction was read from aiodeepseek's _pow.cpp
// and then confirmed against a live server challenge: the server accepted
// nonce 20941 for
//
//	salt a7bee800cede26f66764, expire_at 1790022592780
//	-> 8bb415dd09a5593fc1cc4b7a91a8f3039be373065c4260de1b0793e43abd4d3a
//
// See pow_test.go for that vector. This implementation is pure Go with no
// dependencies, so the "new deps need justification" rule in specs/README.md is
// satisfied by not needing one.

// dsRate is the sponge rate in bytes: 1600/8 - 2*256/8.
const dsRate = 136

var dsRC = [24]uint64{
	0x0000000000000001, 0x0000000000008082, 0x800000000000808a, 0x8000000080008000,
	0x000000000000808b, 0x0000000080000001, 0x8000000080008081, 0x8000000000008009,
	0x000000000000008a, 0x0000000000000088, 0x0000000080008009, 0x000000008000000a,
	0x000000008000808b, 0x800000000000008b, 0x8000000000008089, 0x8000000000008003,
	0x8000000000008002, 0x8000000000000080, 0x000000000000800a, 0x800000008000000a,
	0x8000000080008081, 0x8000000000008080, 0x0000000080000001, 0x8000000080008008,
}

var dsRotc = [24]uint{1, 3, 6, 10, 15, 21, 28, 36, 45, 55, 2, 14, 27, 41, 56, 8, 25, 43, 62, 18, 39, 61, 20, 44}
var dsPiln = [24]int{10, 7, 11, 17, 18, 3, 5, 16, 8, 21, 24, 4, 15, 23, 19, 13, 12, 2, 20, 14, 22, 9, 6, 1}

// dsKeccakF applies the 23-round permutation: constants RC[1]..RC[23].
func dsKeccakF(a *[25]uint64) {
	var bc [5]uint64
	for r := 1; r <= 23; r++ {
		for i := 0; i < 5; i++ {
			bc[i] = a[i] ^ a[i+5] ^ a[i+10] ^ a[i+15] ^ a[i+20]
		}
		for i := 0; i < 5; i++ {
			t := bc[(i+4)%5] ^ bits.RotateLeft64(bc[(i+1)%5], 1)
			for j := 0; j < 25; j += 5 {
				a[j+i] ^= t
			}
		}
		t := a[1]
		for i := 0; i < 24; i++ {
			j := dsPiln[i]
			bc[0] = a[j]
			a[j] = bits.RotateLeft64(t, int(dsRotc[i]))
			t = bc[0]
		}
		for j := 0; j < 25; j += 5 {
			for i := 0; i < 5; i++ {
				bc[i] = a[j+i]
			}
			for i := 0; i < 5; i++ {
				a[j+i] ^= (^bc[(i+1)%5]) & bc[(i+2)%5]
			}
		}
		a[0] ^= dsRC[r]
	}
}

// DeepSeekHashV1 hashes the "{salt}_{expire_at}_{nonce}" byte string. msg must be
// shorter than dsRate (136 bytes); the caller enforces that. It absorbs a single
// block, which is all this scheme ever needs.
func DeepSeekHashV1(msg []byte) [32]byte {
	var blk [dsRate]byte
	copy(blk[:], msg)
	blk[len(msg)] = 0x06 // SHA-3 domain separation, not legacy Keccak's 0x01
	blk[dsRate-1] |= 0x80

	var a [25]uint64
	for i := 0; i < dsRate/8; i++ {
		a[i] = binary.LittleEndian.Uint64(blk[i*8:])
	}
	dsKeccakF(&a)

	var out [32]byte
	for i := 0; i < 4; i++ {
		binary.LittleEndian.PutUint64(out[i*8:], a[i])
	}
	return out
}
