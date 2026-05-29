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
	text := "line1\nline2\nline3"
	cut := findSafeCutPoint(text, 12)
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
