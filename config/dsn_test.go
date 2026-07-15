package config

import (
	"strings"
	"testing"
)

func TestBuildDSN(t *testing.T) {
	tests := []struct {
		name               string
		template, pw, db   string
		want               string
	}{
		{
			name:     "injects password and database",
			template: "postgres://backup@db.example.com:25060/postgres?sslmode=require",
			pw:       "s3cr3t",
			db:       "tenant_a",
			want:     "postgres://backup:s3cr3t@db.example.com:25060/tenant_a?sslmode=require",
		},
		{
			name:     "keeps template database when none given",
			template: "postgres://backup@db.example.com/postgres",
			pw:       "pw",
			want:     "postgres://backup:pw@db.example.com/postgres",
		},
		{
			name:     "url-encodes special characters in the password",
			template: "mysql://root@db.example.com:3306/app",
			pw:       "p@ss/w:rd",
			db:       "",
			want:     "mysql://root:p%40ss%2Fw%3Ard@db.example.com:3306/app",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := BuildDSN(tt.template, tt.pw, tt.db)
			if err != nil {
				t.Fatal(err)
			}
			if got != tt.want {
				t.Errorf("BuildDSN = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestValidateDSNTemplate(t *testing.T) {
	if err := ValidateDSNTemplate("postgres://backup@host/db?sslmode=require"); err != nil {
		t.Errorf("valid template rejected: %v", err)
	}
	if err := ValidateDSNTemplate(""); err == nil {
		t.Error("empty template accepted")
	}
	err := ValidateDSNTemplate("postgres://backup:leak@host/db")
	if err == nil {
		t.Fatal("template with inline password accepted")
	}
	if !strings.Contains(err.Error(), "password") {
		t.Errorf("error should mention the password: %v", err)
	}
}
