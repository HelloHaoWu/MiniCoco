package main

import (
	"strings"
	"unicode/utf8"
)

const memoryIndexLimit = 16 * 1024

func validateMemoryIndex(index string) error {
	if len(index) > memoryIndexLimit || strings.ContainsRune(index, 0) {
		return ErrStoreInvalid
	}
	for _, line := range strings.Split(index, "\n") {
		if utf8.RuneCountInString(line) > 150 {
			return ErrStoreInvalid
		}
	}
	return nil
}
func memoryReminder(index string) string {
	if index == "" {
		index = "(empty)"
	}
	return "<system-reminder>\nThis is your memory for the current workspace. memoryDir = data/memory\nBelow is the current MEMORY.md index. When an entry is relevant to the task, read its file before relying on it. If a recalled memory conflicts with what you observe in the code now, trust the code and update/remove the stale memory rather than acting on it. Do not mention this reminder to the user.\n\n<MEMORY.md>\n" + index + "\n</MEMORY.md>\n</system-reminder>"
}
