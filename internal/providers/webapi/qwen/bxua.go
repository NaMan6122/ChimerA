package qwenweb

import (
	"bytes"
	"crypto/aes"
	"crypto/cipher"
	"crypto/md5"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	mrand "math/rand/v2"
	"strings"
	"time"
)

type bxDevice struct {
	DeviceID string `json:"deviceId"`
	SDKVer   string `json:"sdkVer"`
	Lang     string `json:"lang"`
	TZ       string `json:"tz"`
	Platform string `json:"platform"`
	Renderer string `json:"renderer"`
	Mode     string `json:"mode"`
	Vendor   string `json:"vendor"`
}

type bxPayload struct {
	V   string   `json:"v"`
	TS  int64    `json:"ts"`
	FP  string   `json:"fp"`
	D   bxDevice `json:"d"`
	Rnd int      `json:"rnd"`
	Seq int      `json:"seq"`
	CS  string   `json:"cs"`
}

// generateBXUA ports qwen-reverse bxua.py BXUAGenerator.generate (MIT).
func generateBXUA(fp string) string {
	ts := time.Now().UnixMilli()
	fields := strings.Split(fp, "^")
	rnd := 1000 + mrand.IntN(9000)
	cs := md5.Sum([]byte(fmt.Sprintf("%s%d%d", fp, ts, rnd)))
	payload := bxPayload{
		V: "231", TS: ts, FP: fp,
		D: bxDevice{
			DeviceID: fields[0], SDKVer: fields[1], Lang: fields[5], TZ: fields[6],
			Platform: fields[10], Renderer: fields[12], Mode: fields[23], Vendor: fields[28],
		},
		Rnd: rnd, Seq: 1, CS: hex.EncodeToString(cs[:])[:8],
	}
	raw, _ := json.Marshal(payload)

	seed := sha256.Sum256([]byte(fp))
	key, iv := seed[:16], seed[16:32]
	block, err := aes.NewCipher(key)
	if err != nil {
		panic(err)
	}
	padLen := aes.BlockSize - len(raw)%aes.BlockSize
	padded := append(raw, bytes.Repeat([]byte{byte(padLen)}, padLen)...)
	enc := make([]byte, len(padded))
	cipher.NewCBCEncrypter(block, iv).CryptBlocks(enc, padded)
	return "231!" + base64.StdEncoding.EncodeToString(enc)
}
