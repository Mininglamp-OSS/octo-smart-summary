package pipeline

import (
	"embed"
	"log"
	"regexp"
	"strings"
	"sync"

	"gopkg.in/yaml.v3"
)

//go:embed intent_rules.yaml
var rulesFS embed.FS

// IntentRules 意图识别规则配置
type IntentRules struct {
	TimeWords            []string `yaml:"time_words"`
	TimePatterns         []string `yaml:"time_patterns"`
	PersonWords          []string `yaml:"person_words"`
	PersonPatterns       []string `yaml:"person_patterns"`
	ComplexChannelWords  []string `yaml:"complex_channel_words"`
	SimpleChannelWords   []string `yaml:"simple_channel_words"`
	GenericTopics        []string `yaml:"generic_topics"`
	GenericPatterns      []string `yaml:"generic_patterns"`
}

var (
	rulesOnce     sync.Once
	cachedRules   *IntentRules
	timeRegexps   []*regexp.Regexp
	personRegexps []*regexp.Regexp
	genericRegexps []*regexp.Regexp
)

// loadRules 加载规则配置（单例）
func loadRules() *IntentRules {
	rulesOnce.Do(func() {
		data, err := rulesFS.ReadFile("intent_rules.yaml")
		if err != nil {
			log.Printf("[intent_shortcut] WARN: YAML load failed, fallback to default rules (regex disabled): %v", err)
			cachedRules = defaultRules()
			return
		}
		
		var rules IntentRules
		if err := yaml.Unmarshal(data, &rules); err != nil {
			log.Printf("[intent_shortcut] WARN: YAML parse failed, fallback to default rules (regex disabled): %v", err)
			cachedRules = defaultRules()
			return
		}
		cachedRules = &rules
		
		// 预编译正则
		for _, p := range rules.TimePatterns {
			re, err := regexp.Compile(p)
			if err != nil {
				log.Printf("[intent_shortcut] invalid time pattern %q: %v", p, err)
				continue
			}
			timeRegexps = append(timeRegexps, re)
		}
		for _, p := range rules.PersonPatterns {
			re, err := regexp.Compile(p)
			if err != nil {
				log.Printf("[intent_shortcut] invalid person pattern %q: %v", p, err)
				continue
			}
			personRegexps = append(personRegexps, re)
		}
		for _, p := range rules.GenericPatterns {
			re, err := regexp.Compile(p)
			if err != nil {
				log.Printf("[intent_shortcut] invalid generic pattern %q: %v", p, err)
				continue
			}
			genericRegexps = append(genericRegexps, re)
		}
	})
	return cachedRules
}

// defaultRules 默认规则（配置文件加载失败时使用）
func defaultRules() *IntentRules {
	return &IntentRules{
		TimeWords: []string{
			"本周", "最近", "周报", "一周", "上周", "过去", "昨天", "日报",
			"至今", "每周", "下周", "这周", "两周", "今天", "近期", "一个月",
		},
		PersonWords: []string{
			"我的", "我在", "我做", "我和", "我推", "和我", "跟我", "发给我",
			"大家", "团队", "每个人",
		},
		ComplexChannelWords: []string{
			"这些群", "这几个群", "这两个群", "私聊", "私信",
		},
		GenericTopics: []string{
			"总结", "总结一下", "总结主题", "概要",
		},
	}
}

// hasTimeWord 检测是否包含时间词
func hasTimeWord(topic string) bool {
	rules := loadRules()
	topicLower := strings.ToLower(topic)
	
	// 固定词匹配
	for _, word := range rules.TimeWords {
		if strings.Contains(topicLower, strings.ToLower(word)) {
			return true
		}
	}
	
	// 正则匹配
	for _, re := range timeRegexps {
		if re.MatchString(topic) {
			return true
		}
	}
	
	return false
}

// hasPersonWord 检测是否包含人物词
func hasPersonWord(topic string) bool {
	rules := loadRules()
	topicLower := strings.ToLower(topic)
	
	// 固定词匹配
	for _, word := range rules.PersonWords {
		if strings.Contains(topicLower, strings.ToLower(word)) {
			return true
		}
	}
	
	// 正则匹配（@mentions）
	for _, re := range personRegexps {
		if re.MatchString(topic) {
			return true
		}
	}
	
	return false
}

// hasComplexChannelWord 检测是否包含复杂频道词
func hasComplexChannelWord(topic string) bool {
	rules := loadRules()
	topicLower := strings.ToLower(topic)
	
	for _, word := range rules.ComplexChannelWords {
		if strings.Contains(topicLower, strings.ToLower(word)) {
			return true
		}
	}
	
	return false
}

// isPureGenericTopic 检测是否为纯泛主题
func isPureGenericTopic(topic string) bool {
	rules := loadRules()
	topicClean := strings.TrimSpace(topic)
	topicLower := strings.ToLower(topicClean)
	
	// 完全匹配
	for _, generic := range rules.GenericTopics {
		if topicLower == strings.ToLower(generic) {
			return true
		}
	}
	
	// 正则匹配
	for _, re := range genericRegexps {
		if re.MatchString(topicClean) {
			return true
		}
	}
	
	return false
}

// ShouldSkipIntentRecognition 判断是否可以跳过意图识别 LLM 调用
// 返回 true 表示可以跳过，直接使用默认参数
func ShouldSkipIntentRecognition(topic string, specifiedSources []string, enableShortcut bool) bool {
	// 开关检测
	if !enableShortcut {
		return false
	}
	
	// 条件 1: 纯泛主题
	if isPureGenericTopic(topic) {
		return true
	}
	
	// 条件 2: 有指定来源 + 无时间词 + 无人物词 + 无复杂频道词
	if len(specifiedSources) > 0 &&
		!hasTimeWord(topic) &&
		!hasPersonWord(topic) &&
		!hasComplexChannelWord(topic) {
		return true
	}
	
	return false
}

// GetSkipReason 获取跳过原因（用于日志）
func GetSkipReason(topic string, specifiedSources []string) string {
	if isPureGenericTopic(topic) {
		return "pure_generic_topic"
	}
	if len(specifiedSources) > 0 &&
		!hasTimeWord(topic) &&
		!hasPersonWord(topic) &&
		!hasComplexChannelWord(topic) {
		return "simple_channel_constraint"
	}
	return ""
}
