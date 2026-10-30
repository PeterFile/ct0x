package skills

import (
	"bytes"
	"fmt"
	"io"
	"regexp"
	"strings"
	"unicode/utf8"

	"go.yaml.in/yaml/v3"
)

var skillName = regexp.MustCompile(`^[a-z0-9]+(-[a-z0-9]+)*$`)

// splitHeader only delimits frontmatter. YAML values are parsed by yaml.v3.
func splitHeader(data []byte, limit int64) ([]byte, int, error) {
	offset, start := 0, 0
	for n, line := range bytes.SplitAfter(data, []byte("\n")) {
		end := offset + len(line)
		if int64(end) > limit {
			return nil, 0, ErrLimit
		}
		marker := bytes.TrimSuffix(bytes.TrimSuffix(line, []byte("\n")), []byte("\r"))
		if n == 0 {
			if string(marker) != "---" {
				return nil, 0, invalid("缺少 YAML 头部")
			}
			start = end
		} else if string(marker) == "---" {
			return data[start:offset], end, nil
		}
		offset = end
	}
	return nil, 0, invalid("缺少 YAML 头部结束标记")
}

func stringMap(n *yaml.Node) (map[string]*yaml.Node, error) {
	if n.Kind != yaml.MappingNode || n.Tag != "!!map" || n.Anchor != "" {
		return nil, invalid("必须使用字符串键的映射；不支持锚点或别名")
	}
	out := make(map[string]*yaml.Node)
	for j := 0; j < len(n.Content); j += 2 {
		key := n.Content[j]
		if key.Kind != yaml.ScalarNode || key.Tag != "!!str" || key.Anchor != "" || out[key.Value] != nil {
			return nil, invalid("YAML 键重复或不是字符串")
		}
		out[key.Value] = n.Content[j+1]
	}
	return out, nil
}

func parseHeader(data []byte, parent string) (Metadata, error) {
	var doc, extra yaml.Node
	decoder := yaml.NewDecoder(bytes.NewReader(data))
	if !utf8.Valid(data) || decoder.Decode(&doc) != nil || len(doc.Content) != 1 || decoder.Decode(&extra) != io.EOF {
		return Metadata{}, invalid("YAML 头部必须是单个有效文档")
	}
	fields, err := stringMap(doc.Content[0])
	if err != nil {
		return Metadata{}, err
	}
	m := Metadata{Extra: make(map[string]string)}
	stringsByName := map[string]*string{"name": &m.Name, "description": &m.Description, "license": &m.License, "compatibility": &m.Compatibility, "allowed-tools": &m.AllowedTools}
	for key, value := range fields {
		if key == "metadata" {
			nested, err := stringMap(value)
			if err != nil {
				return Metadata{}, err
			}
			for k, v := range nested {
				if !yamlString(v) {
					return Metadata{}, invalid("metadata 值必须是字符串")
				}
				m.Extra[k] = v.Value
			}
		} else if target := stringsByName[key]; target != nil && yamlString(value) {
			*target = value.Value
		} else {
			return Metadata{}, invalid("头部字段未知或不是字符串")
		}
	}
	if len(m.Name) > 64 || !skillName.MatchString(m.Name) || m.Name != parent || !sizedText(m.Description, 1024) {
		return Metadata{}, invalid("名称、父目录或描述不符合规范")
	}
	if _, exists := fields["compatibility"]; exists && !sizedText(m.Compatibility, 500) {
		return Metadata{}, invalid("compatibility 长度必须为 1–500 个字符")
	}
	return m, nil
}

func yamlString(n *yaml.Node) bool {
	return n.Kind == yaml.ScalarNode && n.Tag == "!!str" && n.Anchor == "" && utf8.ValidString(n.Value)
}
func sizedText(s string, max int) bool {
	return strings.TrimSpace(s) != "" && utf8.ValidString(s) && utf8.RuneCountInString(s) <= max
}
func invalid(reason string) error { return fmt.Errorf("%w：%s", ErrInvalid, reason) }
