package server

import (
	"strings"
	"testing"
	"unicode/utf8"
)

func TestSplitMessage_ShortText(t *testing.T) {
	text := "短文本"
	chunks := splitMessage(text, 4000)
	if len(chunks) != 1 || chunks[0] != text {
		t.Fatalf("短文本应原样返回, got %v", chunks)
	}
}

func TestSplitMessage_UTF8Boundary(t *testing.T) {
	// 生成一段全是中文的长文本
	text := strings.Repeat("你好世界", 500) // 每个中文 3 字节，20*500=10000 字节
	chunks := splitMessage(text, 1000)

	for i, chunk := range chunks {
		// 去掉 [n/m] 前缀后检查
		content := chunk
		if idx := strings.Index(chunk, " "); idx > 0 && chunk[0] == '[' {
			content = chunk[idx+1:]
		}
		if !utf8.ValidString(content) {
			t.Fatalf("chunk %d 不是有效 UTF-8: %q", i, content[:50])
		}
	}
}

func TestSplitMessage_ParagraphBoundary(t *testing.T) {
	para1 := strings.Repeat("第一段内容。", 50) // ~750 字节
	para2 := strings.Repeat("第二段内容。", 50) // ~750 字节
	text := para1 + "\n\n" + para2

	chunks := splitMessage(text, 800)

	if len(chunks) < 2 {
		t.Fatalf("应该在段落边界切割, got %d chunks", len(chunks))
	}

	// 第一个 chunk 应包含第一段（不含第二段）
	if !strings.Contains(chunks[0], "第一段") {
		t.Fatalf("第一个 chunk 应包含第一段内容")
	}
}

func TestSplitMessage_TableIntegrity(t *testing.T) {
	header := "| 列A | 列B |\n|------|------|\n"
	row := "| 数据1 | 数据2 |\n"
	text := strings.Repeat(header, 1) + strings.Repeat(row, 100) // ~2500+ 字节

	chunks := splitMessage(text, 800)

	for i, chunk := range chunks {
		content := chunk
		if idx := strings.Index(chunk, " "); idx > 0 && chunk[0] == '[' {
			content = chunk[idx+1:]
		}
		// 检查所有表格行都是完整的（以 | 开头和结尾）
		lines := strings.Split(content, "\n")
		for _, line := range lines {
			trimmed := strings.TrimSpace(line)
			if trimmed == "" {
				continue
			}
			if trimmed[0] == '|' {
				// 表格行必须以 | 结尾
				if trimmed[len(trimmed)-1] != '|' {
					t.Fatalf("chunk %d 表格行被截断: %q", i, trimmed)
				}
			}
		}
	}
}

func TestSplitMessage_SentenceBoundary(t *testing.T) {
	text := strings.Repeat("这是一段很长的句子，用来测试句号切割。", 50) // ~2050 字节

	chunks := splitMessage(text, 800)

	for i, chunk := range chunks {
		content := chunk
		if idx := strings.Index(chunk, " "); idx > 0 && chunk[0] == '[' {
			content = chunk[idx+1:]
		}
		if !utf8.ValidString(content) {
			t.Fatalf("chunk %d 不是有效 UTF-8", i)
		}
	}

	if len(chunks) < 2 {
		t.Fatalf("应该被切割成多段, got %d", len(chunks))
	}
}

func TestFindSafeCutPoint_NoMultiByte(t *testing.T) {
	// 纯 ASCII 文本在换行符处切割
	// "line1\n" = 6 bytes, "line2\n" = 6 bytes, "line3" = 5 bytes
	// maxBytes=11 → text[:11] = "line1\nline2"，最后一个 \n 在位置 5，cut=6
	text := "line1\nline2\nline3"
	cut := findSafeCutPoint(text, 11)
	expected := 6 // "line1\n" = 6 bytes
	if cut != expected {
		t.Fatalf("expected cut at %d, got %d", expected, cut)
	}
}

func TestFindSafeCutPoint_UTF8Fallback(t *testing.T) {
	// 无换行、无句号的纯中文文本，应在 UTF-8 边界切割
	text := strings.Repeat("你好", 100) // 600 字节
	cut := findSafeCutPoint(text, 100)

	// 确保切割点是有效的 UTF-8 边界
	prefix := text[:cut]
	if !utf8.ValidString(prefix) {
		t.Fatalf("切割点 %d 不是 UTF-8 边界", cut)
	}

	// 确保不超过限制
	if cut > 100 {
		t.Fatalf("切割点 %d 超过限制 100", cut)
	}
}

func TestSplitMessage_CodeBlockIntegrity(t *testing.T) {
	// 构建一个包含代码块的长消息
	code := strings.Repeat("fmt.Println(\"hello world\")\n", 150) // ~4500 字节的代码
	text := "这是一段说明文字。\n\n```go\n" + code + "```\n\n这是代码后面的文字。"

	chunks := splitMessage(text, 2000)

	for i, chunk := range chunks {
		content := chunk
		if idx := strings.Index(chunk, " "); idx > 0 && chunk[0] == '[' {
			content = chunk[idx+1:]
		}

		// 检查代码块是否闭合
		inCode := false
		lineStart := 0
		for j := 0; j <= len(content); j++ {
			if j == len(content) || content[j] == '\n' {
				trimmed := strings.TrimSpace(content[lineStart:j])
				if strings.HasPrefix(trimmed, "```") {
					inCode = !inCode
				}
				lineStart = j + 1
			}
		}
		if inCode {
			t.Fatalf("chunk %d 有未闭合的代码块", i)
		}
	}
}

func TestSplitMessage_CodeBlockNotSplit(t *testing.T) {
	// 短代码块不应被拆分
	text := "前置文字\n\n```python\nprint('hello')\nprint('world')\n```\n\n后置文字"
	chunks := splitMessage(text, 50)

	for i, chunk := range chunks {
		content := chunk
		if idx := strings.Index(chunk, " "); idx > 0 && chunk[0] == '[' {
			content = chunk[idx+1:]
		}
		// 每个 chunk 都不应该有未闭合的代码块
		if hasUnclosedCodeBlock(content) {
			t.Fatalf("chunk %d 有未闭合的代码块: %q", i, content[:80])
		}
	}
}

func TestFindCodeBlockRanges(t *testing.T) {
	text := "before\n```go\ncode\n```\nafter\n```js\nmore\n```\nend"
	ranges := findCodeBlockRanges(text)

	if len(ranges) != 2 {
		t.Fatalf("expected 2 code blocks, got %d", len(ranges))
	}

	// 第一个代码块应包含 ```go ... ```
	if !strings.Contains(text[ranges[0][0]:ranges[0][1]], "```go") {
		t.Fatalf("first range should contain ```go")
	}
	// 第二个代码块应包含 ```js ... ```
	if !strings.Contains(text[ranges[1][0]:ranges[1][1]], "```js") {
		t.Fatalf("second range should contain ```js")
	}
}

func TestHasUnclosedCodeBlock(t *testing.T) {
	tests := []struct {
		text     string
		expected bool
	}{
		{"```go\ncode\n```", false},
		{"```go\ncode\n", true},
		{"no code here", false},
		{"```\ncode\n```\n```\nmore", true},
	}

	for _, tt := range tests {
		got := hasUnclosedCodeBlock(tt.text)
		if got != tt.expected {
			t.Errorf("hasUnclosedCodeBlock(%q) = %v, want %v", tt.text, got, tt.expected)
		}
	}
}

func TestExtractCodeBlockLang(t *testing.T) {
	tests := []struct {
		text     string
		expected string
	}{
		{"```go\ncode", "go"},
		{"```\ncode\n```\n```python\nmore", "python"},
		{"```\ncode\n```", ""},
		{"no code", ""},
	}

	for _, tt := range tests {
		got := extractCodeBlockLang(tt.text)
		if got != tt.expected {
			t.Errorf("extractCodeBlockLang(%q) = %q, want %q", tt.text, got, tt.expected)
		}
	}
}
