package domain

import "testing"

func TestParseStartAttribution(t *testing.T) {
	tests := []struct {
		payload  string
		ok       bool
		format   string
		platform string
	}{
		{"v_f1_yt", true, "f1", "yt"},
		{"v_f3_vk", true, "f3", "vk"},
		{"v_f1_yt_shorts", true, "f1", "yt_shorts"}, // «_» внутри площадки
		{"v_promo-jan_ig", true, "promo-jan", "ig"},
		{"", false, "", ""},
		{"v_", false, "", ""},
		{"v_f1", false, "", ""},          // нет площадки
		{"v_f1_", false, "", ""},         // пустая площадка
		{"v__yt", false, "", ""},         // пустой формат
		{"ref_123", false, "", ""},       // чужой префикс
		{"promo_XXX", false, "", ""},     // чужой префикс
		{"link_abc", false, "", ""},      // чужой префикс
		{"v_F1_yt", false, "", ""},       // верхний регистр
		{"v_f1_у-тюб", false, "", ""},    // кириллица
		{"v_f1_yt!", false, "", ""},      // спецсимвол
	}
	for _, tt := range tests {
		att, ok := ParseStartAttribution(tt.payload)
		if ok != tt.ok {
			t.Errorf("ParseStartAttribution(%q) ok = %v, want %v", tt.payload, ok, tt.ok)
			continue
		}
		if !ok {
			continue
		}
		if att.Format != tt.format || att.Platform != tt.platform || att.Payload != tt.payload {
			t.Errorf("ParseStartAttribution(%q) = %+v, want format=%q platform=%q", tt.payload, att, tt.format, tt.platform)
		}
	}
}
