// Copyright 2021-present The Atlas Authors. All rights reserved.
// This source code is licensed under the Apache 2.0 license found
// in the LICENSE file in the root directory of this source tree.

package cmdext_test

import (
	"context"
	"encoding/base64"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"ariga.io/atlas/cmd/atlas/internal/cmdext"
	"ariga.io/atlas/schemahcl"
	"ariga.io/atlas/sql/migrate"
	"ariga.io/atlas/sql/sqlclient"
	_ "ariga.io/atlas/sql/sqlite"
	"ariga.io/atlas/sql/sqltool"

	_ "github.com/mattn/go-sqlite3"
	"github.com/stretchr/testify/require"
	"github.com/zclconf/go-cty/cty"
)

func TestRuntimeVarSrc(t *testing.T) {
	var (
		v struct {
			V string `spec:"v"`
		}
		state = schemahcl.New(cmdext.DataSources...)
	)
	err := state.EvalBytes([]byte(`
data "runtimevar" "pass" {
  url = "constant://?val=hello+world&decoder=binary"
}

v = data.runtimevar.pass
`), &v, nil)
	require.EqualError(t, err, `data.runtimevar.pass: unsupported decoder: "binary"`)

	err = state.EvalBytes([]byte(`
data "runtimevar" "pass" {
  url = "constant://?val=hello+world"
}

v = data.runtimevar.pass
`), &v, nil)
	require.NoError(t, err)
	require.Equal(t, v.V, "hello world")

	err = state.EvalBytes([]byte(`
data "runtimevar" "pass" {
  url = "constant://?val=hello+world&decoder=string"
}

v = data.runtimevar.pass
`), &v, nil)
	require.NoError(t, err, "nop decoder")
	require.Equal(t, v.V, "hello world")
}

func TestQuerySrc(t *testing.T) {
	ctx := context.Background()
	u := fmt.Sprintf("sqlite3://file:%s?cache=shared&_fk=1", filepath.Join(t.TempDir(), "test.db"))
	drv, err := sqlclient.Open(context.Background(), u)
	require.NoError(t, err)
	_, err = drv.ExecContext(ctx, "CREATE TABLE users (name text)")
	require.NoError(t, err)
	_, err = drv.ExecContext(ctx, "INSERT INTO users (name) VALUES ('a8m')")
	require.NoError(t, err)

	var (
		v struct {
			C  int      `spec:"c"`
			V  string   `spec:"v"`
			Vs []string `spec:"vs"`
		}
		state = schemahcl.New(cmdext.DataSources...)
	)
	err = state.EvalBytes([]byte(fmt.Sprintf(`
data "sql" "user" {
  url   = %q
  query = "SELECT name FROM users"
}

c = data.sql.user.count
v = data.sql.user.value
vs = data.sql.user.values
`, u)), &v, nil)
	require.NoError(t, err)
	require.Equal(t, 1, v.C)
	require.Equal(t, "a8m", v.V)
	require.Equal(t, []string{"a8m"}, v.Vs)
}

func TestEntLoader_LoadState(t *testing.T) {
	ctx := context.Background()
	drv, err := sqlclient.Open(ctx, "sqlite://test?mode=memory&_fk=1")
	require.NoError(t, err)
	u, err := url.Parse("ent://../migrate/ent/schema")
	require.NoError(t, err)
	l, ok := cmdext.States.Loader("ent")
	require.True(t, ok)
	state, err := l.LoadState(ctx, &cmdext.LoadStateOptions{
		Dev:  drv,
		URLs: []*url.URL{u},
	})
	require.NoError(t, err)
	realm, err := state.ReadState(ctx)
	require.NoError(t, err)
	require.Len(t, realm.Schemas, 1)
	require.Len(t, realm.Schemas[0].Tables, 1)
	revT := realm.Schemas[0].Tables[0]
	require.Equal(t, "atlas_schema_revisions", revT.Name)
}

func TestEntLoader_MigrateDiff(t *testing.T) {
	ctx := context.Background()
	drv, err := sqlclient.Open(ctx, "sqlite://test?mode=memory&_fk=1")
	require.NoError(t, err)
	d, ok := cmdext.States.Differ([]string{"ent://../migrate/ent/schema?globalid=1"})
	require.True(t, ok)

	t.Run("AtlasFormat", func(t *testing.T) {
		dir, err := migrate.NewLocalDir(t.TempDir())
		require.NoError(t, err)
		err = d.MigrateDiff(ctx, &cmdext.MigrateDiffOptions{
			Name:   "boring",
			Indent: "\t",
			Dev:    drv,
			Dir:    dir,
			To:     []string{"ent://../migrate/ent/schema?globalid=1"},
		})
		require.NoError(t, err)
		files, err := dir.Files()
		require.NoError(t, err)
		require.True(t, strings.HasSuffix(files[0].Name(), "_boring.sql"))
		// Statements were generated with indentation.
		require.Contains(t, string(files[0].Bytes()), "CREATE TABLE `atlas_schema_revisions` (\n\t")
	})

	t.Run("OtherFormat", func(t *testing.T) {
		dir, err := sqltool.NewGolangMigrateDir(t.TempDir())
		require.NoError(t, err)
		err = d.MigrateDiff(ctx, &cmdext.MigrateDiffOptions{
			Name: "boring",
			Dev:  drv,
			Dir:  dir,
			To:   []string{"ent://../migrate/ent/schema?globalid=1"},
		})
		require.NoError(t, err)
		files, err := dir.Files()
		require.NoError(t, err)
		require.True(t, strings.HasSuffix(files[0].Name(), "_boring.up.sql"))
	})

	t.Run("Invalid", func(t *testing.T) {
		_, ok := cmdext.States.Differ([]string{"ent://../migrate/ent/schema"})
		require.False(t, ok, "skipping schemas without globalid")
	})
}

func TestTemplateDir(t *testing.T) {
	var (
		v struct {
			Dir string `spec:"dir"`
		}
		dir   = t.TempDir()
		state = schemahcl.New(cmdext.DataSources...)
	)
	err := os.WriteFile(filepath.Join(dir, "1.sql"), []byte("create table {{ .Schema }}.t(c int);"), 0644)
	require.NoError(t, err)
	err = state.EvalBytes([]byte(`
variable "path" {
  type = string
}

data "template_dir" "tenant" {
  path = var.path
  vars = {
    Schema = "a8m"
  }
}

dir = data.template_dir.tenant.url
`), &v, map[string]cty.Value{
		"path": cty.StringVal(dir),
	})
	require.NoError(t, err)
	require.NotEmpty(t, v.Dir)
	d := migrate.OpenMemDir(strings.TrimPrefix(v.Dir, "mem://"))
	require.NoError(t, migrate.Validate(d))
	files, err := d.Files()
	require.NoError(t, err)
	require.Len(t, files, 1)
	require.Equal(t, "1.sql", files[0].Name())
	require.Equal(t, "create table a8m.t(c int);", string(files[0].Bytes()))
}

func TestAtlasConfig(t *testing.T) {
	var (
		v struct {
			Env       string   `spec:"env"`
			HasClient bool     `spec:"has_client"`
			CloudKeys []string `spec:"cloud_keys"`
		}
		state = schemahcl.New(append(cmdext.DataSources, schemahcl.WithVariables(map[string]cty.Value{
			"atlas": cty.ObjectVal(map[string]cty.Value{
				"env": cty.StringVal("dev"),
			}),
		}))...)
	)
	err := state.EvalBytes([]byte(`
atlas {
  cloud {
    url = "url"
    token = "token"
  }
}

env = atlas.env
has_client = atlas.cloud != null
cloud_keys = keys(atlas.cloud)
`), &v, map[string]cty.Value{})
	require.NoError(t, err)
	require.Equal(t, "dev", v.Env)
	require.True(t, v.HasClient)
	require.Equal(t, []string{"client"}, v.CloudKeys, "token and url should not be exported")
}

func TestRemoteDir(t *testing.T) {
	var (
		v struct {
			Dir string `spec:"dir"`
		}
		token string
		state = schemahcl.New(cmdext.DataSources...)
		srv   = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			token = r.Header.Get("Authorization")
			d := migrate.MemDir{}
			if err := d.WriteFile("1.sql", []byte("create table t(c int);")); err != nil {
				http.Error(w, err.Error(), http.StatusInternalServerError)
			}
			arch, err := migrate.ArchiveDir(&d)
			if err != nil {
				http.Error(w, err.Error(), http.StatusInternalServerError)
			}
			fmt.Fprintf(w, `{"data":{"dir":{"content":%q}}}`, base64.StdEncoding.EncodeToString(arch))
		}))
	)
	defer srv.Close()

	err := state.EvalBytes([]byte(`
data "remote_dir" "hello" {
  name  = "atlas"
}
`), &v, map[string]cty.Value{"cloud_url": cty.StringVal(srv.URL)})
	require.EqualError(t, err, "data.remote_dir.hello: missing atlas cloud config")

	err = state.EvalBytes([]byte(`
variable "cloud_url" {
  type = string
}

atlas {
  cloud {
    token = "token"
    url = var.cloud_url
  }
}

data "remote_dir" "hello" {
  name  = "atlas"
}

dir = data.remote_dir.hello.url
`), &v, map[string]cty.Value{"cloud_url": cty.StringVal(srv.URL)})
	require.NoError(t, err)
	require.Equal(t, "Bearer token", token)

	md := migrate.OpenMemDir(strings.TrimPrefix(v.Dir, "mem://"))
	defer md.Close()
	files, err := md.Files()
	require.NoError(t, err)
	require.Len(t, files, 1)
	require.Equal(t, "1.sql", files[0].Name())
	require.Equal(t, "create table t(c int);", string(files[0].Bytes()))
}
