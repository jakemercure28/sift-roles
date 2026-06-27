package voice

import (
	"os"
	"path/filepath"
	"regexp"
	"testing"
)

// TestAppQuestionsVoiceCheckParity guards that the Codex app-questions skill and
// the Claude app-questions command keep the same voice-check gate, so the two
// harnesses stay in step. Ported from the former root test/boilerplate-parity.test.js.
func TestAppQuestionsVoiceCheckParity(t *testing.T) {
	repoRoot := filepath.Join("..", "..")

	read := func(rel string) string {
		b, err := os.ReadFile(filepath.Join(repoRoot, rel))
		if err != nil {
			t.Fatalf("read %s: %v", rel, err)
		}
		return string(b)
	}

	claudeCommand := read(".claude/commands/app-questions.md")
	codexSkill := read(".codex/skills/app-questions/skill.md")

	requiredPhrases := []string{
		"## Step 4: Voice-check all answers",
		`docker compose exec go-backend /server voice-check "ANSWER_TEXT"`,
		"If Sapling score >= 50% AI",
		"If HuggingFace score >= 30% AI",
		"Include voice check results",
	}

	for _, phrase := range requiredPhrases {
		re := regexp.MustCompile(regexp.QuoteMeta(phrase))
		if !re.MatchString(claudeCommand) {
			t.Errorf("Claude command missing voice-check phrase: %q", phrase)
		}
		if !re.MatchString(codexSkill) {
			t.Errorf("Codex skill missing voice-check phrase: %q", phrase)
		}
	}
}
