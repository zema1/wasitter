# wasitter

[English](README.md) ·
[文档](https://pkg.go.dev/github.com/zema1/wasitter) ·
[下载](https://github.com/zema1/wasitter/releases)

wasitter 是一个 Go 的  [Tree-sitter](https://tree-sitter.github.io/tree-sitter/) 实现，
它并非不是从零开始的 Port，而是将 Tree-sitter 的 C 运行时和各语言的语法解析器编译成 WebAssembly，
并在 Go 程序内通过 [wazero](https://wazero.io/) 来加载 wasm 并运行。

## 特点

- 使用时不需要 CGO、C 编译器或系统动态库，可以直接用 Go 原生工具链构建和交叉编译
- 跟随上游定期更新，析正确性和可靠性与原版保持一致，不会因重写导致逻辑错误和维护负担
- 将不同语言的支持外置为独立的 WASM 文件可按需加载，而不是默认携带所有语言
- 针对原版接口做轻量封装，使用体感更符合 Go 语言习惯
- 通过[对照测试](comparison/README.md) 确保关键行为和原版保持一致


> WASM 执行和跨边界调用会带来额外开销，性能与直接使用原生绑定会略有下降，这是预期的


## 快速开始

需要 **Go 1.23 或更新版本**。

```sh
go get github.com/zema1/wasitter@latest
```

项目内置 (embed) 了 JavaScript/JSON 的解析器，可以用下面的代码快速上手

```go
package main

import (
	"context"
	"fmt"
	"log"

	"github.com/zema1/wasitter"
)

func main() {
	if err := run(); err != nil {
		log.Fatal(err)
	}
}

func run() error {
	ctx := context.Background()
	parser, runtime, err := wasitter.NewJavaScriptParser(ctx)
	if err != nil {
		return err
	}
	defer runtime.Close()
	defer parser.Close()

	source := []byte(`function greet(name) { return "Hello, " + name; }`)
	tree, err := parser.ParseContext(ctx, source, nil)
	if err != nil {
		return err
	}
	defer tree.Close()

	function := tree.RootNode().NamedChild(0)
	name := function.ChildByFieldName("name")
	fmt.Println(name.Content(source)) // greet
	return nil
}
```

这里的 runtime 负责运行语言模块，parser 将源代码解析成 tree。
随后从树中取出函数节点，读取它的 `name` 字段。
如果要解析 JSON，使用 `wasitter.NewJSONParser(ctx)` 创建解析器即可。

## 加载其他语言


其他语言的 WASM 文件可以从 [Releases](https://github.com/zema1/wasitter/releases) 下载，
版本应与项目中使用的 wasitter 版本一致，否则会有未定义行为。

目前支持的语言包括 `Bash`、`C`、`C++`、`Go`、`Java`、`Python`、`Ruby`、`Rust`、`TypeScript`、`TSX` 等 12 中语言

以 Python 为例：把 **`wasitter-python.wasm`** 下载到合适位置，如 `grammars/wasitter-python.wasm`
然后就可以这样加载使用:

```go
parser, runtime, err := wasitter.NewParserFromFile(ctx, "grammars/wasitter-python.wasm")
if err != nil {
	return err
}
defer runtime.Close()
defer parser.Close()

source := []byte("def greet(name):\n    return name\n")
```


这个入口会从 WASM 模块中读取对应的语法，并返回一个可以直接使用的解析器。
接下来和前面的例子一样调用 `ParseContext` 即可。如果已经通过 `go:embed` 拿到
WASM 字节，可以直接用 `wasitter.NewParserFromWASM(ctx, wasmBytes)`。

## 使用建议

- 处理多个文件时，可以复用 parser 和 runtime，每个文件解析得到的树在用完后关闭。
  要并行解析，则为每个工作协程分别创建 runtime 和 parser。
- 读取节点时，不要关闭它所在的树。树、查询和解析器都用完后，再关闭 runtime。
  上面示例中的 `defer` 已按这个顺序安排。
- 解析成功不代表代码没有语法错误。Tree-sitter 会尽量为不完整的代码生成语法树，
  是否存在语法错误需要另外检查 `tree.RootNode().HasError()`。
- 节点的位置按 UTF-8 字节计算，列号也是字节数，不是字符数或 UTF-16 代码单元数。

查询、增量解析、runtime 配置及兼容性说明，集中放在[使用指南](USAGE.md)中。

## 参与开发

如果要添加语言、重新构建 WASM，
或了解测试和发布流程，可以阅读 [DEVELOPMENT.md](DEVELOPMENT.md)。
安全漏洞请按[安全策略](SECURITY.md)中的方式报告。

## 许可证

wasitter 使用 [MIT 许可证](LICENSE)。依赖的语法、运行时和工具链各自的许可信息，
见[第三方声明](internal/wasm/THIRD_PARTY_NOTICES.md)。
分发 WASM 文件时，请附上 Release 中的 `THIRD_PARTY_NOTICES.txt`。
