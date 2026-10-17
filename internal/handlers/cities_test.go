package handlers

import (
	"testing"
)

func TestResolveCityCodeAndName(t *testing.T) {
	tests := []struct {
		name         string
		inCode       string
		inName       string
		inLoc        string
		wantCode     string
		wantName     string
		wantLoc      string
	}{
		{
			name:     "标准6位代码与空名称",
			inCode:   "330100",
			inName:   "",
			inLoc:    "西湖断桥",
			wantCode: "330100",
			wantName: "杭州市",
			wantLoc:  "西湖断桥",
		},
		{
			name:     "无代码从地点反查杭州",
			inCode:   "",
			inName:   "",
			inLoc:    "西湖苏堤（杭州市）",
			wantCode: "330100",
			wantName: "杭州市",
			wantLoc:  "西湖苏堤（杭州市）",
		},
		{
			name:     "无代码从简称反查大理",
			inCode:   "",
			inName:   "大理古城",
			inLoc:    "",
			wantCode: "532900",
			wantName: "大理白族自治州",
			wantLoc:  "",
		},
		{
			name:     "非法脏代码清洗",
			inCode:   "INVALID_CODE",
			inName:   "",
			inLoc:    "上海黄浦江畔",
			wantCode: "310100",
			wantName: "上海市",
			wantLoc:  "上海黄浦江畔",
		},
		{
			name:     "完全不包含城市地名",
			inCode:   "",
			inName:   "",
			inLoc:    "自习室角落",
			wantCode: "",
			wantName: "",
			wantLoc:  "自习室角落",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			gotCode, gotName, gotLoc := ResolveCityCodeAndName(tt.inCode, tt.inName, tt.inLoc)
			if gotCode != tt.wantCode {
				t.Errorf("gotCode = %v, want %v", gotCode, tt.wantCode)
			}
			if gotName != tt.wantName {
				t.Errorf("gotName = %v, want %v", gotName, tt.wantName)
			}
			if gotLoc != tt.wantLoc {
				t.Errorf("gotLoc = %v, want %v", gotLoc, tt.wantLoc)
			}
		})
	}
}
