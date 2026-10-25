package config

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

func TestConfig(t *testing.T) {
	RegisterFailHandler(Fail)
	RunSpecs(t, "Config Suite")
}

var _ = Describe("Load", func() {
	var dir string

	BeforeEach(func() {
		dir = GinkgoT().TempDir()
	})

	It("loads a valid config and applies it", func() {
		path := filepath.Join(dir, "config.yaml")
		Expect(os.WriteFile(path, []byte(`
llm:
  base_url: "http://127.0.0.1:8080/v1"
  api_key_file: "./api-key"
  model: "test-model"
  timeout_seconds: 30
  reasoning_effort: "high"
log:
  level: "debug"
`), 0o600)).To(Succeed())

		cfg, err := Load(path)

		Expect(err).NotTo(HaveOccurred())
		Expect(cfg.LLM.BaseURL).To(Equal("http://127.0.0.1:8080/v1"))
		Expect(cfg.LLM.Model).To(Equal("test-model"))
		Expect(cfg.LLM.TimeoutSeconds).To(Equal(30))
		Expect(cfg.LLM.ReasoningEffort).To(Equal("high"))
		Expect(cfg.Log.Level).To(Equal("debug"))
	})

	It("applies defaults for missing fields", func() {
		path := filepath.Join(dir, "config.yaml")
		Expect(os.WriteFile(path, []byte("llm:\n  model: \"m\"\n"), 0o600)).To(Succeed())

		cfg, err := Load(path)

		Expect(err).NotTo(HaveOccurred())
		Expect(cfg.LLM.BaseURL).To(Equal("http://127.0.0.1:8080/v1"))
		Expect(cfg.LLM.TimeoutSeconds).To(Equal(120))
		Expect(cfg.LLM.ReasoningEffort).To(Equal("medium"))
	})

	It("returns an error when the file does not exist", func() {
		_, err := Load(filepath.Join(dir, "missing.yaml"))

		Expect(err).To(HaveOccurred())
	})
})

var _ = Describe("Config.APIKey", func() {
	It("reads and trims the api key file", func() {
		dir := GinkgoT().TempDir()
		keyPath := filepath.Join(dir, "api-key")
		Expect(os.WriteFile(keyPath, []byte("secret-key\n"), 0o600)).To(Succeed())

		cfg := &Config{LLM: LLMConfig{APIKeyFile: keyPath}}
		key, err := cfg.APIKey()

		Expect(err).NotTo(HaveOccurred())
		Expect(key).To(Equal("secret-key"))
	})

	It("returns empty string when no file is configured", func() {
		cfg := &Config{}
		key, err := cfg.APIKey()

		Expect(err).NotTo(HaveOccurred())
		Expect(key).To(Equal(""))
	})
})

var _ = Describe("Timezone", func() {
	var dir string

	BeforeEach(func() {
		dir = GinkgoT().TempDir()
	})

	load := func(extra string) (*Config, error) {
		path := filepath.Join(dir, "config.yaml")
		Expect(os.WriteFile(path, []byte("llm:\n  model: \"m\"\n"+extra), 0o600)).To(Succeed())
		return Load(path)
	}

	It("returns time.Local when empty", func() {
		cfg, err := load("")
		Expect(err).NotTo(HaveOccurred())
		loc, err := cfg.Location()
		Expect(err).NotTo(HaveOccurred())
		Expect(loc).To(Equal(time.Local))
	})

	It("loads Europe/Belgrade", func() {
		cfg, err := load("agent:\n  timezone: \"Europe/Belgrade\"\n")
		Expect(err).NotTo(HaveOccurred())
		loc, err := cfg.Location()
		Expect(err).NotTo(HaveOccurred())
		Expect(loc.String()).To(Equal("Europe/Belgrade"))
	})

	It("fails to load an invalid timezone", func() {
		_, err := load("agent:\n  timezone: \"Not/AZone\"\n")
		Expect(err).To(HaveOccurred())
	})
})
