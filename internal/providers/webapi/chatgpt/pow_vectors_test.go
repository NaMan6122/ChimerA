package chatgptweb

// Code generated from the chat2api Python reference with a FIXED config.
// These pin the Go SHA3-512 PoW to externally computed vectors — the same
// discipline the DeepSeek solver uses, where a self-consistent but wrong hash
// would otherwise pass every local test and fail only against the live server.
//
// Regenerate with scripts/chatgptweb-spike/keys_gen.py; do not edit by hand.

type powVector struct {
	seed, diff, want string
	solved bool
}

var powVectors = []powVector{
	{seed: "0.123456", diff: "0fffff", want: "WzMwMDgsIk1vbiBKYW4gMDIgMjAyNiAxMDowMDowMCBHTVQtMDUwMCAoRWFzdGVybiBTdGFuZGFyZCBUaW1lKSIsNDI5NDcwNTE1MiwxMiwiTW96aWxsYS81LjAgdGVzdCIsIiIsIiIsImVuLVVTIiwiZW4tVVMsZXMtVVMsZW4sZXMiLDYsIndlYmRyaXZlcuKIkmZhbHNlIiwibG9jYXRpb24iLCJ3aW5kb3ciLDEyMy40NTYsIjAwMDAwMDAwLTAwMDAtNDAwMC04MDAwLTAwMDAwMDAwMDAwMCIsIiIsOCw5OTkuMF0=", solved: true},
	{seed: "0.5", diff: "000032", want: "WzMwMDgsIk1vbiBKYW4gMDIgMjAyNiAxMDowMDowMCBHTVQtMDUwMCAoRWFzdGVybiBTdGFuZGFyZCBUaW1lKSIsNDI5NDcwNTE1MiwxMTU3OTIsIk1vemlsbGEvNS4wIHRlc3QiLCIiLCIiLCJlbi1VUyIsImVuLVVTLGVzLVVTLGVuLGVzIiw1Nzg5Niwid2ViZHJpdmVy4oiSZmFsc2UiLCJsb2NhdGlvbiIsIndpbmRvdyIsMTIzLjQ1NiwiMDAwMDAwMDAtMDAwMC00MDAwLTgwMDAtMDAwMDAwMDAwMDAwIiwiIiw4LDk5OS4wXQ==", solved: true},
	{seed: "seed-abc", diff: "0f", want: "WzMwMDgsIk1vbiBKYW4gMDIgMjAyNiAxMDowMDowMCBHTVQtMDUwMCAoRWFzdGVybiBTdGFuZGFyZCBUaW1lKSIsNDI5NDcwNTE1Miw1NiwiTW96aWxsYS81LjAgdGVzdCIsIiIsIiIsImVuLVVTIiwiZW4tVVMsZXMtVVMsZW4sZXMiLDI4LCJ3ZWJkcml2ZXLiiJJmYWxzZSIsImxvY2F0aW9uIiwid2luZG93IiwxMjMuNDU2LCIwMDAwMDAwMC0wMDAwLTQwMDAtODAwMC0wMDAwMDAwMDAwMDAiLCIiLDgsOTk5LjBd", solved: true},
}
