package video

import "testing"

func TestSanitizeVideoID(t *testing.T) {
	value, err := sanitizeVideoID("Lesson_42-ABC")
	if err != nil {
		t.Fatalf("sanitize video id: %v", err)
	}
	if value != "lesson_42-abc" {
		t.Fatalf("unexpected sanitized value: %s", value)
	}
}

func TestSanitizeVideoIDRejectsSlash(t *testing.T) {
	if _, err := sanitizeVideoID("../bad"); err == nil {
		t.Fatalf("expected error for unsafe id")
	}
}
