package incctx

import (
	"context"
	"fmt"
	"io"
	"os"
	"strings"
)

func readTailRange(ctx context.Context, path string, offset, limit int) (any, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	file, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("file_read: %w", err)
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil {
		return nil, err
	}
	startByte := max(int64(0), info.Size()-maxScanBytes)
	if _, err := file.Seek(startByte, io.SeekStart); err != nil {
		return nil, err
	}
	data, err := io.ReadAll(io.LimitReader(file, maxScanBytes))
	if err != nil {
		return nil, err
	}
	text := string(data)
	if startByte > 0 {
		boundary := strings.IndexByte(text, '\n')
		if boundary < 0 {
			return nil, fmt.Errorf("file_read: tail line exceeds scan limit")
		}
		text = text[boundary+1:]
	}
	lines := strings.Split(text, "\n")
	start := len(lines) + offset
	if start < 0 {
		return nil, fmt.Errorf("file_read: negative offset exceeds available bounded tail")
	}
	end := min(len(lines), start+limit)
	for end > start && len(strings.Join(lines[start:end], "\n")) > maxOutputBytes {
		end--
	}
	if end == start {
		return nil, fmt.Errorf("file_read: tail line exceeds output limit")
	}
	for _, line := range lines[start:end] {
		if len(line) > maxLineBytes {
			return nil, fmt.Errorf("file_read: tail line exceeds line limit")
		}
	}
	result := map[string]any{"path": path, "content": strings.Join(lines[start:end], "\n"), "offset": offset, "next_offset": offset + end - start, "truncated": end < len(lines), "total_lines": nil}
	if startByte == 0 {
		result["start_line"], result["end_line"], result["total_lines"] = start+1, end, len(lines)
	}
	return result, nil
}
