package panel

import (
	"os"
	"path/filepath"
	"testing"
)

// 前端已按视图拆成 js/*.js 多个文件（见 index.go 的 jsLoadOrder）。
// 但绝大多数前端测试的断言方式是「从 app.js 里切一段函数出来在 node 里跑」，
// 逐个改成读多文件既啰嗦又容易切错边界。
//
// 这里在测试进程启动时把全部文件按加载顺序拼成一个临时的 app.js（内容与拆分前
// 的等价），测试结束删除。测试代码因此完全不用改，断言的仍是「拼接后的整体」。
//
// 注意：拼接顺序必须与 jsLoadOrder 一致，否则顶层 `start()` 会早于其他模块。
func TestMain(m *testing.M) {
	code := func() int {
		var buf []byte
		for _, name := range jsLoadOrder {
			raw, err := os.ReadFile(filepath.Join("js", name+".js"))
			if err != nil {
				// 读不到就不生成：依赖它的测试会自然失败并报出清晰原因。
				return m.Run()
			}
			buf = append(buf, raw...)
			buf = append(buf, '\n')
		}
		if err := os.WriteFile("app.js", buf, 0o644); err != nil {
			return m.Run()
		}
		defer os.Remove("app.js")
		return m.Run()
	}()
	os.Exit(code)
}
