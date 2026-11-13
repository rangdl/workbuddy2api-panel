package main

import (
	"fmt"
	"os"

	"github.com/linguo2625469/workbuddy2api-panel/internal/auth"
)

func main() {
	auths, err := auth.LoadDir(os.Args[1])
	if err != nil {
		fmt.Println("err:", err)
		return
	}
	for _, a := range auths {
		fmt.Printf("%-12s realm=%-6s global=%-5v enterprise=%-5v entSession=%-5v refresh=%v\n",
			a.NicknameValue(), a.Realm(), a.IsGlobal(), a.IsEnterprise(),
			a.IsEnterpriseSession(), a.RefreshTokenValue() != "")
	}
}
