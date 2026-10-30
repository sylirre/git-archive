// SPDX-FileCopyrightText: 2026 Sylirre <107305601+sylirre@users.noreply.github.com>
// SPDX-License-Identifier: Apache-2.0

package gitx

import (
	"fmt"
	"strings"
)

// keepGitEnv lists the GIT_* variables a user may legitimately set to
// adapt transport behaviour (proxies, CA bundles, ssh command, tracing).
// Every other GIT_* variable is dropped: variables such as GIT_DIR,
// GIT_WORK_TREE, GIT_INDEX_FILE or GIT_CONFIG_PARAMETERS would redirect
// operations to unrelated repositories or inject configuration, which is
// a common accident when the tool runs from inside a git hook.
var keepGitEnv = map[string]bool{
	"GIT_SSH":                   true,
	"GIT_SSH_COMMAND":           true,
	"GIT_SSH_VARIANT":           true,
	"GIT_SSL_CAINFO":            true,
	"GIT_SSL_CAPATH":            true,
	"GIT_SSL_CERT":              true,
	"GIT_SSL_KEY":               true,
	"GIT_PROXY_COMMAND":         true,
	"GIT_PROXY_SSL_CAINFO":      true,
	"GIT_PROXY_SSL_CERT":        true,
	"GIT_PROXY_SSL_KEY":         true,
	"GIT_HTTP_PROXY_AUTHMETHOD": true,
	"GIT_HTTP_USER_AGENT":       true,
	"GIT_CONFIG_GLOBAL":         true,
	"GIT_CONFIG_SYSTEM":         true,
	"GIT_CONFIG_NOSYSTEM":       true,
	"GIT_EXEC_PATH":             true,
	"GIT_CURL_VERBOSE":          true,
}

// dropEnv lists non-GIT_* variables that are replaced or must not leak
// into git: locale settings (git output is parsed and must be English),
// askpass programs (would pop up dialogs during unattended runs).
var dropEnv = map[string]bool{
	"LANG":                true,
	"LANGUAGE":            true,
	"SSH_ASKPASS":         true,
	"SSH_ASKPASS_REQUIRE": true,
	"GCM_INTERACTIVE":     true,
}

// fixedEnv is appended to every git invocation.
var fixedEnv = []string{
	// Parsing relies on untranslated messages.
	"LC_ALL=C",
	// Never wait for a password on a terminal or in a dialog; fail fast.
	"GIT_TERMINAL_PROMPT=0",
	"GIT_ASKPASS=", // empty, not unset: disables core.askPass and SSH_ASKPASS too
	"SSH_ASKPASS_REQUIRE=never",
	"GCM_INTERACTIVE=never",
	// LFS content is fetched explicitly and only when enabled; never as a
	// side effect of checkout.
	"GIT_LFS_SKIP_SMUDGE=1",
	// Report progress immediately rather than after two seconds.
	"GIT_PROGRESS_DELAY=0",
	"GIT_PAGER=cat",
}

// cleanEnv derives the environment for git from the process environment.
func cleanEnv(environ []string) []string {
	out := make([]string, 0, len(environ)+len(fixedEnv))
	for _, kv := range environ {
		k, _, ok := strings.Cut(kv, "=")
		if !ok {
			continue
		}
		if strings.HasPrefix(k, "GIT_") && !keepGitEnv[k] && !strings.HasPrefix(k, "GIT_TRACE") {
			continue
		}
		if strings.HasPrefix(k, "LC_") || dropEnv[k] {
			continue
		}
		out = append(out, kv)
	}
	return append(out, fixedEnv...)
}

// benignFsck are fsck message IDs that flag historical formatting quirks
// (bad dates, zero-padded file modes written by ancient git versions,
// sloppy author lines) found in many real repositories. They are ignored
// so that such repositories can be archived. Checks that guard against
// malicious trees (".git", "..", .gitmodules abuse, duplicate entries,
// symlinked special files) keep their default severity.
var benignFsck = []string{
	"badDate",
	"badDateOverflow",
	"badEmail",
	"badFilemode",
	"badTagName",
	"badTimezone",
	"missingEmail",
	"missingNameBeforeEmail",
	"missingSpaceBeforeDate",
	"missingSpaceBeforeEmail",
	"missingTaggerEntry",
	"multipleAuthors",
	"zeroPaddedDate",
	"zeroPaddedFilemode",
}

// hardening is passed as "-c" options to every git invocation. Command
// line configuration has the highest precedence, so neither the user's
// global configuration nor anything inside a repository can relax it.
var hardening = []string{
	// Never execute hooks or a filesystem monitor, whatever a repository
	// configuration or template might say.
	"core.hooksPath=/dev/null",
	"core.fsmonitor=false",
	// Reject paths that alias ".git" on NTFS or HFS+, so that an archive
	// copied to another system stays safe to check out.
	"core.protectNTFS=true",
	"core.protectHFS=true",
	// Only discover repositories through explicit --git-dir; never treat
	// a bare repository embedded in a checkout as a repository.
	"safe.bareRepository=explicit",
	// Validate every received object; this rejects malicious trees
	// (entries named ".git" or "..", hostile .gitmodules) before they are
	// stored or checked out.
	"transfer.fsckObjects=true",
	"fetch.fsckObjects=true",
	// Submodules can point anywhere; they are never followed.
	"submodule.recurse=false",
	"fetch.recurseSubmodules=false",
	// No templates: new repositories contain nothing we did not fetch.
	"init.templateDir=",
	"core.askPass=",
	"credential.interactive=false",
	"color.ui=false",
	"advice.detachedHead=false",
	// Automatic maintenance runs in the foreground, inside our timeout
	// and process group, rather than as an orphaned background process.
	"gc.autoDetach=false",
	"maintenance.autoDetach=false",
	// Every transport is forbidden unless explicitly allowed below.
	"protocol.allow=never",
}

// configArgs returns the "-c" arguments for the given allowed protocols
// and credentials.
func configArgs(protocols []string, creds []Credential) []string {
	args := make([]string, 0, 2*(len(hardening)+2*len(benignFsck)+len(protocols)+2*len(creds)))
	for _, kv := range hardening {
		args = append(args, "-c", kv)
	}
	for _, id := range benignFsck {
		args = append(args, "-c", "fsck."+id+"=ignore", "-c", "fetch.fsck."+id+"=ignore")
	}
	for _, p := range protocols {
		args = append(args, "-c", "protocol."+p+".allow=always")
	}
	for i, c := range creds {
		key := "credential." + c.URLPrefix + ".helper"
		// An empty value resets the helper list for this URL, so the
		// token is the only credential offered to that host. The secret
		// itself travels through the environment, never through argv.
		args = append(args, "-c", key+"=", "-c", key+"="+credentialHelper(i))
	}
	return args
}

func credentialHelper(i int) string {
	return fmt.Sprintf(`!f() { test "$1" = get || exit 0; printf 'username=%%s\npassword=%%s\n' "$GIT_ARCHIVE_USER_%d" "$GIT_ARCHIVE_TOKEN_%d"; }; f`, i, i)
}

func credentialEnv(creds []Credential) []string {
	env := make([]string, 0, 2*len(creds))
	for i, c := range creds {
		user := c.Username
		if user == "" {
			user = "x-access-token"
		}
		env = append(env, fmt.Sprintf("GIT_ARCHIVE_USER_%d=%s", i, user), fmt.Sprintf("GIT_ARCHIVE_TOKEN_%d=%s", i, c.Token))
	}
	return env
}
