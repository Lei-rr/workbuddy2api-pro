// tabs_test.go 钉住账号池的三个二级 tab：账号池 / 模型锁池 / 积分到期。
//
// 用户明确要求这三个 tab 必须保留（1000+ 账号下用 tab 分屏：账号表、模型级限流、
// 到期批次各自独立，一屏放三张表会挤成一团）。本文件只钉「结构与切换契约」：
// HTML 里三个 tab 存在且顺序为 账号池 → 模型锁池 → 积分到期；app.js 的
// switchAccTab/expBoxShow 把可见性绑定到对应容器。各 tab 的渲染细节由既有用例覆盖。
package panel

import (
	"os"
	"strings"
	"testing"
)

func TestPanelAccountPoolTabs(t *testing.T) {
	html, err := os.ReadFile("index.html")
	if err != nil {
		t.Fatal(err)
	}
	h := string(html)

	// 容器与三个按钮（顺序即用户要求的展示顺序）。
	for _, must := range []string{
		`<div class="subtabs" id="accTabs">`,
		`data-tab="list">账号池</button>`,
		`data-tab="models">模型锁池</button>`,
		`data-tab="expiry">积分到期</button>`,
		`id="accBox"`,
		`id="mlBox"`,
		`id="expBox"`,
	} {
		if !strings.Contains(h, must) {
			t.Errorf("index.html 缺少账号池 tab 结构 %q（三个 tab 必须保留）", must)
		}
	}
	iList := strings.Index(h, `data-tab="list"`)
	iModels := strings.Index(h, `data-tab="models"`)
	iExpiry := strings.Index(h, `data-tab="expiry"`)
	if !(iList >= 0 && iModels > iList && iExpiry > iModels) {
		t.Errorf("tab 顺序应为 账号池→模型锁池→积分到期，got list=%d models=%d expiry=%d", iList, iModels, iExpiry)
	}

	// v2.6.1：app.js 由 web/*.js 拼接后嵌入，测试统一从拼接源码取。
	js, err := appJSReadFile()
	if err != nil {
		t.Fatal(err)
	}
	j := string(js)
	for _, must := range []string{
		"function switchAccTab(tab) {",
		"accTab = (tab === 'expiry' || tab === 'models') ? tab : 'list';",
		"if ($('expBox')) $('expBox').hidden = accTab !== 'expiry';",
		"if ($('accBox')) $('accBox').hidden = accTab !== 'list';",
		"if ($('mlBox')) $('mlBox').hidden = accTab !== 'models';",
		"switchAccTab(b.dataset.tab)",
	} {
		if !strings.Contains(j, must) {
			t.Errorf("app.js 缺少 tab 切换/可见性契约 %q", must)
		}
	}
}
