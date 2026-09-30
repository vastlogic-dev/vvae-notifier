package notifier

// 需要落盘的状态：Docker 版存在挂载卷里，每个部署串一个 state-<通道标识>.json。

import (
	"encoding/json"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
)

type SavedConfig struct {
	V    int    `json:"v"`
	Blob string `json:"blob"`
}

type State struct {
	// 最近一次拉到的配置密文与版本；启动时解密恢复，PAT 不以明文落盘。
	Cfg *SavedConfig `json:"cfg"`
	// 轮询源与凭据的指纹，变了就重新记基线。
	SourceKey string   `json:"sourceKey"`
	BaselineT *int64   `json:"baselineT"`
	Seen      []string `json:"seen"`
	ETag      string   `json:"etag"`
	// 已为当前失效发过系统提醒，恢复前不重复。
	InvalidNotified bool `json:"invalidNotified"`
	// 已发过的过期提醒档位（天数）。
	Warned []int `json:"warned"`
	// 下次查 PAT 过期时间，Unix 秒。
	NextTokenCheck int64 `json:"nextTokenCheck"`
	// 待发往中继的密文，中继暂时不可达时保留，最多 50 条。
	Outbox []string `json:"outbox"`
	// 实例 id（协议 inst），首次启动生成，换轮询源时也保留。
	Inst string `json:"inst,omitempty"`
}

const (
	seenLimit   = 500
	outboxLimit = 50
)

type Store interface {
	// Load 总是返回可用的状态；读不到或格式不对时返回空状态和原因。
	Load() (*State, error)
	Save(*State) error
}

// FileStore 把状态存成 <dir>/<name>，先写临时文件再改名，不会留下写了一半的文件。
type FileStore struct{ dir, name string }

func NewFileStore(dir, name string) *FileStore { return &FileStore{dir, name} }

func (f *FileStore) path() string { return filepath.Join(f.dir, f.name) }

func (f *FileStore) Load() (*State, error) {
	data, err := os.ReadFile(f.path())
	if errors.Is(err, fs.ErrNotExist) {
		return &State{}, nil
	}
	if err != nil {
		return &State{}, err
	}
	var s State
	if err := json.Unmarshal(data, &s); err != nil {
		return &State{}, err
	}
	return &s, nil
}

func (f *FileStore) Save(s *State) error {
	data, err := json.Marshal(s)
	if err != nil {
		return err
	}
	tmp := f.path() + ".tmp"
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, f.path())
}

// Writable 检查状态目录能否写入，启动时用来给出明确提示。
func (f *FileStore) Writable() error {
	if err := os.MkdirAll(f.dir, 0o700); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(f.dir, ".probe-*")
	if err != nil {
		return err
	}
	tmp.Close()
	return os.Remove(tmp.Name())
}
