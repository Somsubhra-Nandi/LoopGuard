package config

import (
	"bytes"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"

	"gopkg.in/yaml.v3"
)

// Edit changes one key of a config file: a top-level key, or a dotted
// path into nested mappings ("notify.ntfy.topic").
type Edit struct {
	Key    string
	Value  string // a plain YAML scalar
	Remove bool
}

const newFileHeader = "# LoopGuard configuration: see docs/LLD.md §12 for every key.\n" +
	"# Written by loopguard (init, mode); comments and other keys are kept.\n"

// EditFile applies edits to the keys of the YAML file at path,
// creating it if needed. Comments, key order, and other keys are kept
// (the file is re-encoded, so indentation is normalized). The result must
// still be a valid config; a file that isn't valid YAML is refused,
// untouched. The file is replaced atomically.
func EditFile(path string, edits ...Edit) error {
	old, err := os.ReadFile(path)
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	var doc yaml.Node
	if len(bytes.TrimSpace(old)) > 0 {
		if err := yaml.Unmarshal(old, &doc); err != nil {
			return fmt.Errorf("%s is not valid YAML, not editing it: %w", path, err)
		}
	}
	if doc.Kind == 0 {
		doc = yaml.Node{Kind: yaml.DocumentNode, Content: []*yaml.Node{{Kind: yaml.MappingNode}}}
		if len(old) == 0 {
			doc.HeadComment = newFileHeader
		}
	}
	if doc.Kind != yaml.DocumentNode || len(doc.Content) != 1 || doc.Content[0].Kind != yaml.MappingNode {
		return fmt.Errorf("%s: the top level is not a mapping, not editing it", path)
	}
	for _, e := range edits {
		m, key := doc.Content[0], e.Key
		// A dotted key ("notify.ntfy.topic") goes through nested mappings,
		// created as needed; a scalar in the way (`ntfy: true`) is replaced.
		for {
			parent, rest, nested := strings.Cut(key, ".")
			if !nested {
				break
			}
			if m = child(m, parent, !e.Remove); m == nil {
				break
			}
			key = rest
		}
		if m == nil {
			continue // removing a key whose parent doesn't exist
		}
		i := -1
		for k := 0; k+1 < len(m.Content); k += 2 {
			if m.Content[k].Value == key {
				i = k
				break
			}
		}
		switch {
		case e.Remove && i >= 0:
			m.Content = append(m.Content[:i], m.Content[i+2:]...)
		case e.Remove:
		case i >= 0:
			v := m.Content[i+1]
			*v = yaml.Node{Kind: yaml.ScalarNode, Value: e.Value, LineComment: v.LineComment, HeadComment: v.HeadComment}
		default:
			m.Content = append(m.Content,
				&yaml.Node{Kind: yaml.ScalarNode, Value: key},
				&yaml.Node{Kind: yaml.ScalarNode, Value: e.Value})
		}
	}
	var b bytes.Buffer
	enc := yaml.NewEncoder(&b)
	enc.SetIndent(2)
	if err := enc.Encode(&doc); err != nil {
		return err
	}
	if err := enc.Close(); err != nil {
		return err
	}
	if _, _, err := Parse(b.Bytes()); err != nil {
		return fmt.Errorf("the edited %s would be invalid: %w", filepath.Base(path), err)
	}
	return writeAtomic(path, b.Bytes())
}

// child returns the mapping under key in m, creating it (or replacing a
// non-mapping value) when create is set; nil otherwise.
func child(m *yaml.Node, key string, create bool) *yaml.Node {
	for k := 0; k+1 < len(m.Content); k += 2 {
		if m.Content[k].Value != key {
			continue
		}
		v := m.Content[k+1]
		if v.Kind == yaml.MappingNode {
			if v.Style&yaml.FlowStyle != 0 {
				v.Style = 0 // block style, so added keys get their own lines
			}
			return v
		}
		if !create {
			return nil
		}
		*v = yaml.Node{Kind: yaml.MappingNode, HeadComment: v.HeadComment, LineComment: v.LineComment}
		return v
	}
	if !create {
		return nil
	}
	v := &yaml.Node{Kind: yaml.MappingNode}
	m.Content = append(m.Content, &yaml.Node{Kind: yaml.ScalarNode, Value: key}, v)
	return v
}

func writeAtomic(path string, b []byte) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), filepath.Base(path)+".tmp*")
	if err != nil {
		return err
	}
	_, werr := tmp.Write(b)
	cerr := tmp.Close()
	if err := errors.Join(werr, cerr); err != nil {
		os.Remove(tmp.Name())
		return err
	}
	if err := os.Rename(tmp.Name(), path); err != nil {
		os.Remove(tmp.Name())
		return err
	}
	return nil
}
