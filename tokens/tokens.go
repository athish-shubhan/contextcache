// Package tokens counts tokens with OpenAI's cl100k_base BPE (tiktoken),
// loaded offline from an embedded copy. Different models tokenize somewhat
// differently; cl100k is used as one consistent ruler for "how much context
// does this take".
package tokens

import (
	"sync"

	tiktoken "github.com/pkoukk/tiktoken-go"
	loader "github.com/pkoukk/tiktoken-go-loader"
)

var (
	once sync.Once
	enc  *tiktoken.Tiktoken
	err  error
)

func Count(s string) int {
	once.Do(func() {
		tiktoken.SetBpeLoader(loader.NewOfflineLoader())
		enc, err = tiktoken.GetEncoding("cl100k_base")
	})
	if err != nil {
		return (len(s) + 3) / 4
	}
	return len(enc.Encode(s, nil, nil))
}

// Exact reports whether Count uses the real tokenizer (false = len/4 fallback).
func Exact() bool {
	Count("")
	return err == nil
}
