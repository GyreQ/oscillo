// tree.go 技能包的本地目录视图与导出。
//
// 技能源目录来自两处：expert_selection 块里声明的插件路径，或请求中
// Skill 工具描述的注册表（"skills under folder" ↔ "- name:" 条目）。
// 树/预览/导出都直读该目录；目录不存在时退回流量沉淀的正文。
package skills

import (
	"archive/zip"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// Entry 目录树节点。
type Entry struct {
	Name     string  `json:"name"`
	Path     string  `json:"path"` // 相对技能根目录
	Dir      bool    `json:"dir"`
	Size     int64   `json:"size"`
	Children []Entry `json:"children,omitempty"`
}

const (
	maxTreeFiles   = 500     // 树节点数上限（防误沉淀到巨型目录）
	maxFileBytes   = 2 << 20 // 单文件预览上限 2MiB
	maxExportBytes = 32 << 20 // 导出 zip 总量上限 32MiB
)

// skipFile 目录里跳过的杂物。
func skipFile(name string) bool {
	return name == ".DS_Store" || strings.HasSuffix(name, ".DS_Store")
}

// sourceOf 从 index.jsonl 找该技能最新的本地包目录（无则空串）。
func sourceOf(root, name string) string {
	data, err := os.ReadFile(filepath.Join(root, "skills", "index.jsonl"))
	if err != nil {
		return ""
	}
	src := ""
	for _, line := range strings.Split(string(data), "\n") {
		if line == "" {
			continue
		}
		var u Usage
		if json.Unmarshal([]byte(line), &u) != nil || u.Name != name {
			continue
		}
		if u.Source != "" {
			src = u.Source
		}
	}
	return src
}

// Tree 技能目录树。hasLocal=false 表示本地无包目录，
// 前端应把沉淀正文（SKILL.md 虚拟节点）当作唯一内容。
func Tree(root, name string) (entries []Entry, source string, hasLocal bool) {
	src := sourceOf(root, name)
	if src == "" {
		return nil, "", false
	}
	fi, err := os.Stat(src)
	if err != nil || !fi.IsDir() {
		return nil, src, false
	}
	n := 0
	entries = buildTree(src, "", &n)
	return entries, src, true
}

// buildTree 递归构建（目录在前、字典序）。
func buildTree(dir, rel string, n *int) []Entry {
	des, err := os.ReadDir(dir)
	if err != nil {
		return nil
	}
	var out []Entry
	for _, de := range des {
		if skipFile(de.Name()) || *n >= maxTreeFiles {
			continue
		}
		*n++
		childRel := de.Name()
		if rel != "" {
			childRel = rel + "/" + de.Name()
		}
		if de.IsDir() {
			out = append(out, Entry{
				Name: de.Name(), Path: childRel, Dir: true,
				Children: buildTree(filepath.Join(dir, de.Name()), childRel, n),
			})
		} else {
			info, err := de.Info()
			size := int64(0)
			if err == nil {
				size = info.Size()
			}
			out = append(out, Entry{Name: de.Name(), Path: childRel, Size: size})
		}
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Dir != out[j].Dir {
			return out[i].Dir
		}
		return out[i].Name < out[j].Name
	})
	return out
}

// textExts 可文本预览的扩展名；imageExts 可内联预览的图片。
var textExts = map[string]bool{
	".md": true, ".txt": true, ".py": true, ".sh": true, ".js": true, ".ts": true,
	".json": true, ".html": true, ".css": true, ".yaml": true, ".yml": true,
	".xml": true, ".csv": true, ".toml": true, ".ini": true, ".cfg": true,
	".conf": true, ".log": true, ".go": true, ".rs": true, ".tsv": true,
}
var imageExts = map[string]bool{
	".png": true, ".jpg": true, ".jpeg": true, ".gif": true, ".svg": true,
	".webp": true, ".ico": true,
}

// FileKind 文件内容形态。
type FileKind string

const (
	KindText   FileKind = "text"
	KindImage  FileKind = "image"
	KindBinary FileKind = "binary"
)

// ReadFile 读技能包内一个文件。
// 无本地目录时，rel=="SKILL.md" 返回流量沉淀的正文（虚拟节点）。
func ReadFile(root, name, rel string) (data []byte, kind FileKind, size int64, ok bool) {
	rel = strings.Trim(filepath.Clean("/"+strings.TrimPrefix(rel, "/")), "/")
	if rel == "" || strings.Contains(rel, "..") {
		return nil, "", 0, false
	}
	ext := strings.ToLower(filepath.Ext(rel))
	src := sourceOf(root, name)
	if src != "" {
		if fi, err := os.Stat(src); err == nil && fi.IsDir() {
			full := filepath.Join(src, filepath.FromSlash(rel))
			// 防目录逃逸：解析后必须仍在技能根内
			if !strings.HasPrefix(full, src+string(os.PathSeparator)) {
				return nil, "", 0, false
			}
			data, err := os.ReadFile(full)
			if err != nil || int64(len(data)) > maxFileBytes {
				if err == nil {
					return nil, KindBinary, int64(len(data)), true // 超限只报大小
				}
				return nil, "", 0, false
			}
			switch {
			case imageExts[ext]:
				return data, KindImage, int64(len(data)), true
			case textExts[ext]:
				return data, KindText, int64(len(data)), true
			default:
				return data, KindBinary, int64(len(data)), true
			}
		}
	}
	// 退回流量沉淀正文
	if rel == "SKILL.md" {
		if b, err := os.ReadFile(filepath.Join(root, "skills", fileSafe(name)+".md")); err == nil {
			return b, KindText, int64(len(b)), true
		}
	}
	return nil, "", 0, false
}

// ExportZip 把技能包打成 zip 写入 w；返回打包文件数。
// 本地无目录时打包沉淀正文为单个 SKILL.md。
func ExportZip(root, name string, w io.Writer) (int, error) {
	src := sourceOf(root, name)
	zw := zip.NewWriter(w)
	count, total := 0, int64(0)
	hasDir := false
	if src != "" {
		if fi, err := os.Stat(src); err == nil && fi.IsDir() {
			hasDir = true
			base := src
			filepath.Walk(src, func(p string, fi os.FileInfo, err error) error {
				if err != nil || fi.IsDir() || skipFile(fi.Name()) {
					return nil
				}
				if count >= maxTreeFiles || total+fi.Size() > maxExportBytes {
					return filepath.SkipAll
				}
				rel, err := filepath.Rel(base, p)
				if err != nil {
					return nil
				}
				addFileToZip(zw, filepath.ToSlash(rel), p)
				count++
				total += fi.Size()
				return nil
			})
		}
	}
	if !hasDir {
		if b, err := os.ReadFile(filepath.Join(root, "skills", fileSafe(name)+".md")); err == nil {
			if f, err := zw.Create("SKILL.md"); err == nil {
				f.Write(b)
				count = 1
			}
		}
	}
	return count, zw.Close()
}

func addFileToZip(zw *zip.Writer, rel, path string) {
	f, err := zw.Create(rel)
	if err != nil {
		return
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return
	}
	f.Write(data)
}
