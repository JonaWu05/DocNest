// hashpw 是一個小工具，用來把明文密碼轉成 bcrypt hash，
// 產生的 hash 可填入 .env 的 USERS 設定（格式：username:hash）。
//
// 用法：
//
//	go run ./cmd/hashpw '你的密碼'
package main

import (
	"fmt"
	"os"

	"golang.org/x/crypto/bcrypt"
)

func main() {
	if len(os.Args) < 2 || os.Args[1] == "" {
		fmt.Fprintln(os.Stderr, "用法：go run ./cmd/hashpw '你的密碼'")
		os.Exit(1)
	}

	hash, err := bcrypt.GenerateFromPassword([]byte(os.Args[1]), bcrypt.DefaultCost)
	if err != nil {
		fmt.Fprintln(os.Stderr, "產生 hash 失敗：", err)
		os.Exit(1)
	}

	// 直接印出 hash，方便複製貼到 .env
	fmt.Println(string(hash))
}
