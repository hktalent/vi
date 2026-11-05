//go:build !importface

package main

import (
	"fmt"
	"os"
)

// runImportFaces 是不带 importface build tag 时的 fallback。
// 打印提示信息并退出。
func runImportFaces(args []string) int {
	fmt.Fprintln(os.Stderr, "importfaces 子命令需要编译时加 -tags importface（需要 CGO + OpenCV）")
	fmt.Fprintln(os.Stderr, "本地构建: ./build-importface.sh")
	return 1
}
