package panel

import (
	"bytes"
	"os"
	"os/exec"
	"testing"
)

// TestAccountsGroupByBackend 账号表按上游分组：多上游时才出组头，
// 单上游不渲染（不改变既有观感）。
func TestAccountsGroupByBackend(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node not installed")
	}
	script := `const fs=require('fs'), vm=require('vm');
const src=fs.readFileSync('js/accounts.js','utf8');
function grab(names){let out='';for(const n of names){
  const s=src.indexOf('function '+n+'('); if(s<0) throw new Error('miss '+n);
  let i=src.indexOf('{',s),d=0,e=-1;
  for(;i<src.length;i++){if(src[i]==='{')d++;else if(src[i]==='}'){d--;if(!d){e=i+1;break}}}
  out+=src.slice(s,e)+'\n';}return out;}
const c0=src.indexOf('const BACKEND_LABEL');
const consts=src.slice(c0, src.indexOf('};', c0)+2);
const sb={console,String,Array,Object,esc:s=>String(s==null?'':s)};
vm.createContext(sb);
vm.runInContext(consts+grab(['groupByBackend','backendSectionRow']), sb, {filename:'g.js'});
const assert=(c,m)=>{if(!c){console.log('FAIL: '+m);process.exit(1);}};

// 混合池：两组，保持各自顺序
let g=sb.groupByBackend([
  {uid:'w1',backend:'codebuddy'},{uid:'t1',backend:'trae'},{uid:'w2',backend:'codebuddy'}]);
assert(g.length===2, '应分两组');
assert(g[0].backend==='codebuddy' && g[0].rows.length===2, 'codebuddy 组应有 2 个');
assert(g[1].backend==='trae' && g[1].rows.length===1, 'trae 组应有 1 个');

// 缺 backend 字段（旧数据）默认 codebuddy
g=sb.groupByBackend([{uid:'a'}]);
assert(g.length===1 && g[0].backend==='codebuddy', '缺 backend 应归 codebuddy');

// 未知 backend 不被吞
g=sb.groupByBackend([{uid:'x',backend:'future'}]);
assert(g.length===1 && g[0].backend==='future', '未知上游应保留');

// 组头渲染：名称/账号数/可用数
const row=sb.backendSectionRow('trae',[{uid:'a'},{uid:'b',disabled:true}]);
assert(row.includes('Trae'), '组头应显示上游名');
assert(row.includes('2 个账号'), '组头应显示账号数');
assert(row.includes('1 可用'), '组头应显示可用数');
assert(row.includes('colspan="9"'), '组头应跨全部列');

console.log('GROUP OK'); process.exit(0);
`
	f, err := os.CreateTemp(t.TempDir(), "grp-*.cjs")
	if err != nil {
		t.Fatal(err)
	}
	f.WriteString(script)
	f.Close()
	out, err := exec.Command(node, f.Name()).CombinedOutput()
	if err != nil || !bytes.Contains(out, []byte("GROUP OK")) {
		t.Fatalf("分组渲染失败: %v\n%s", err, out)
	}
}
