// Package config 는 portswap serve 설정을 로드한다.
package config

import (
	"fmt"
	"os"
	"time"

	"gopkg.in/yaml.v3"
)

// Duration 은 YAML 에서 "30s" 같은 문자열을 time.Duration 으로 읽는다.
type Duration time.Duration

func (d *Duration) UnmarshalYAML(value *yaml.Node) error {
	var s string
	if err := value.Decode(&s); err != nil {
		return err
	}
	parsed, err := time.ParseDuration(s)
	if err != nil {
		return fmt.Errorf("duration %q: %w", s, err)
	}
	*d = Duration(parsed)
	return nil
}

// ServiceSpec 은 하나의 서비스 설정이다.
type ServiceSpec struct {
	Listen         string    `yaml:"listen"`
	Health         string    `yaml:"health"`
	HealthTimeout  Duration  `yaml:"health_timeout"`
	HealthInterval Duration  `yaml:"health_interval"`
	Drain          Duration  `yaml:"drain"`
	Grace          Duration  `yaml:"grace"`
	AutoRestart    *bool     `yaml:"auto_restart"`
	Command        []string  `yaml:"command"`
}

// File 은 설정 파일 전체 구조이다.
type File struct {
	Control  string                 `yaml:"control"`
	Services map[string]ServiceSpec `yaml:"services"`
}

// LoadFile 은 YAML 설정 파일을 읽는다.
func LoadFile(path string) (File, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return File{}, err
	}
	var f File
	if err := yaml.Unmarshal(data, &f); err != nil {
		return File{}, fmt.Errorf("설정 해석 실패: %w", err)
	}
	if len(f.Services) == 0 {
		return File{}, fmt.Errorf("services 가 비어 있습니다")
	}
	for name, spec := range f.Services {
		if spec.Listen == "" {
			return File{}, fmt.Errorf("서비스 %q: listen 이 필요합니다", name)
		}
		if len(spec.Command) == 0 {
			return File{}, fmt.Errorf("서비스 %q: command 가 필요합니다", name)
		}
	}
	return f, nil
}

// ApplyDefaults 는 비어 있는 필드에 기본값을 채운다.
func (s *ServiceSpec) ApplyDefaults() {
	if s.HealthTimeout == 0 {
		s.HealthTimeout = Duration(30 * time.Second)
	}
	if s.HealthInterval == 0 {
		s.HealthInterval = Duration(500 * time.Millisecond)
	}
	if s.Drain == 0 {
		s.Drain = Duration(30 * time.Second)
	}
	if s.Grace == 0 {
		s.Grace = Duration(15 * time.Second)
	}
	if s.AutoRestart == nil {
		t := true
		s.AutoRestart = &t
	}
}
