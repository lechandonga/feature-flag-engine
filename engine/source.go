package engine

import (
	"context"
	"os"
)

// FileSource 从本地文件读取配置，用于模拟“配置下发”：
// 替换文件内容即等价于下发了新版本，刷新器会重新解析、校验并原子换入。
type FileSource struct {
	Path string
}

// Load 读取配置文件；返回 (内容, 来源标识, 错误)。
func (s *FileSource) Load(_ context.Context) ([]byte, string, error) {
	data, err := os.ReadFile(s.Path)
	if err != nil {
		return nil, "", err
	}
	return data, s.Path, nil
}

// StaticSource 内存静态数据源，便于测试与本地直配。
type StaticSource struct {
	Data   []byte
	Source string
}

func (s *StaticSource) Load(_ context.Context) ([]byte, string, error) {
	return s.Data, s.Source, nil
}
