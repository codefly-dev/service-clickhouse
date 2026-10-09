package main

import (
	"testing"

	"github.com/codefly-dev/core/resources"
	"github.com/stretchr/testify/require"
)

// The restricted render hands consumers a template; the ephemeral render and
// the local runtime hand them an assembled string. Both are built from the
// same literals and the same URL_USERINFO escape, so for every credential the
// environment's secret store assembles exactly the bytes this agent would have
// assembled had it held the value -- reserved characters included.
func TestConnectionTemplateAssemblesTheSameBytesAsTheConnectionString(t *testing.T) {
	const address, database = "evidence.platform-obin-tracing.svc.cluster.local:9000", "tracing_evidence"
	for _, credential := range []struct{ user, password string }{
		{"user", "password"},
		{"evidence", "a1b2c3d4e5f6"},
		{"ev-id_ence.1", "p@ss:w/rd?&+ 1"},
		{"us@er", "%25already#encoded"},
		{"ünïcode", "pässwörd"},
	} {
		template := clickhouseConnectionTemplate(address, database)
		require.NoError(t, resources.ValidateConfigurationValueTemplate(template))
		assembled, err := resources.EvaluateConfigurationValueTemplate(template, func(configuration, key string) (string, bool) {
			require.Equal(t, "clickhouse", configuration)
			switch key {
			case "CLICKHOUSE_USER":
				return credential.user, true
			case "CLICKHOUSE_PASSWORD":
				return credential.password, true
			}
			return "", false
		})
		require.NoError(t, err)
		require.Equal(t, clickhouseConnectionString(address, database, credential.user, credential.password), assembled)
	}
}

// The assembled form stays a URL a client can parse back to the credential:
// the escape is the one URL userinfo decodes, never a verbatim insertion.
func TestConnectionStringEscapesUserinfo(t *testing.T) {
	require.Equal(t,
		"clickhouse://user:password@host:9000/db",
		clickhouseConnectionString("host:9000", "db", "user", "password"),
		"the unreserved set is unchanged, so existing consumers see the same bytes")
	require.Equal(t,
		"clickhouse://us%40er:p%40ss%2Fw%3Frd@host:9000/db",
		clickhouseConnectionString("host:9000", "db", "us@er", "p@ss/w?rd"))
}
