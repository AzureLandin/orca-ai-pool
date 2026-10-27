import contextlib
import importlib.util
import io
import json
import stat
import tempfile
import unittest
from pathlib import Path
from unittest import mock


def load_poolctl():
    path = Path(__file__).resolve().parent.parent / "scripts" / "poolctl.py"
    spec = importlib.util.spec_from_file_location("poolctl", path)
    if spec is None or spec.loader is None:
        raise RuntimeError("could not load poolctl.py")
    module = importlib.util.module_from_spec(spec)
    spec.loader.exec_module(module)
    return module


poolctl = load_poolctl()


class JailbreakManifestTests(unittest.TestCase):
    def test_disabled_returns_none_without_reading_prompts(self):
        with tempfile.TemporaryDirectory() as tmp:
            self.assertIsNone(poolctl.load_jailbreak_config(Path(tmp), {"enabled": False}))

    def test_compiles_profiles(self):
        with tempfile.TemporaryDirectory() as tmp:
            root = Path(tmp)
            (root / "prompts" / "upstream").mkdir(parents=True)
            (root / "prompts" / "manifest.yaml").write_text(
                """
suffix: "-jailbreak"
unmapped: "error"
default_mode: "append"
profiles:
  - name: "deepseek"
    models: ["deepseek-v4.1-flash", "deepseek/*"]
    file: "upstream/deepseek.md"
""",
                encoding="utf-8",
            )
            (root / "prompts" / "upstream" / "deepseek.md").write_text(
                "PROMPT-BODY\n", encoding="utf-8"
            )

            config = poolctl.load_jailbreak_config(root, {"enabled": True})

            self.assertEqual(config["suffix"], "-jailbreak")
            self.assertEqual(config["unmapped"], "error")
            self.assertEqual(len(config["profiles"]), 1)
            self.assertEqual(config["profiles"][0]["name"], "deepseek")
            self.assertEqual(config["profiles"][0]["mode"], "append")
            self.assertEqual(config["profiles"][0]["prompt"], "PROMPT-BODY")

    def test_missing_prompt_tells_user_to_sync(self):
        with tempfile.TemporaryDirectory() as tmp:
            root = Path(tmp)
            (root / "prompts").mkdir()
            (root / "prompts" / "manifest.yaml").write_text(
                """
profiles:
  - name: "missing"
    models: ["m"]
    file: "upstream/missing.md"
""",
                encoding="utf-8",
            )
            with self.assertRaises(poolctl.PoolctlError) as ctx:
                poolctl.load_jailbreak_config(root, {"enabled": True})
            self.assertIn("sync-jailbreaks", str(ctx.exception))

    def test_overlay_is_prepended(self):
        with tempfile.TemporaryDirectory() as tmp:
            root = Path(tmp)
            (root / "prompts" / "upstream").mkdir(parents=True)
            (root / "prompts" / "overlays").mkdir(parents=True)
            (root / "prompts" / "manifest.yaml").write_text(
                """
profiles:
  - name: "deepseek"
    models: ["deepseek-v4.1-flash"]
    file: "upstream/deepseek.md"
    overlay: "overlays/deepseek.md"
    overlay_position: "prepend"
""",
                encoding="utf-8",
            )
            (root / "prompts" / "upstream" / "deepseek.md").write_text(
                "UPSTREAM-BODY\n", encoding="utf-8"
            )
            (root / "prompts" / "overlays" / "deepseek.md").write_text(
                "ORCA-OVERLAY\n", encoding="utf-8"
            )

            config = poolctl.load_jailbreak_config(root, {"enabled": True})
            self.assertEqual(
                config["profiles"][0]["prompt"],
                "ORCA-OVERLAY\n\nUPSTREAM-BODY",
            )

    def test_missing_overlay_is_rejected(self):
        with tempfile.TemporaryDirectory() as tmp:
            root = Path(tmp)
            (root / "prompts").mkdir()
            (root / "prompts" / "manifest.yaml").write_text(
                """
profiles:
  - name: "deepseek"
    models: ["m"]
    file: "upstream/m.md"
    overlay: "overlays/m.md"
""",
                encoding="utf-8",
            )
            (root / "prompts" / "upstream").mkdir()
            (root / "prompts" / "upstream" / "m.md").write_text("BODY", encoding="utf-8")
            with self.assertRaises(poolctl.PoolctlError):
                poolctl.load_jailbreak_config(root, {"enabled": True})

    def test_rejects_parent_path(self):
        with tempfile.TemporaryDirectory() as tmp:
            root = Path(tmp)
            (root / "prompts").mkdir()
            (root / "prompts" / "manifest.yaml").write_text(
                """
profiles:
  - name: "escape"
    models: ["m"]
    file: "../outside.md"
""",
                encoding="utf-8",
            )
            with self.assertRaises(poolctl.PoolctlError):
                poolctl.load_jailbreak_config(root, {"enabled": True})

    def test_unknown_mode_is_rejected(self):
        with tempfile.TemporaryDirectory() as tmp:
            root = Path(tmp)
            (root / "prompts").mkdir()
            (root / "prompts" / "manifest.yaml").write_text(
                """
profiles:
  - name: "bad"
    models: ["m"]
    file: "upstream/m.md"
    mode: "overwrite"
""",
                encoding="utf-8",
            )
            (root / "prompts" / "upstream").mkdir()
            (root / "prompts" / "upstream" / "m.md").write_text("JB", encoding="utf-8")
            with self.assertRaises(poolctl.PoolctlError):
                poolctl.load_jailbreak_config(root, {"enabled": True})


class CliproxyPluginConfigTests(unittest.TestCase):
    def _env(self):
        return {
            "MASTER_API_KEY": "sk-test-master",
            "CPA_ADMIN_SECRET": "test-admin-secret",
            "OC2API_SERVER_KEY": "sk-oc2api",
            "GATEWAY_INTERNAL_PORT": "18317",
        }

    def test_jailbreak_plugin_is_added(self):
        jailbreak = {
            "suffix": "-jailbreak",
            "unmapped": "error",
            "profiles": [
                {
                    "name": "deepseek",
                    "models": ["deepseek-v4.1-flash"],
                    "mode": "prepend",
                    "prompt": "JB",
                }
            ],
        }
        config = poolctl._build_cliproxy_config(
            self._env(), "pro-only", [], [], [], {"enabled": False}, jailbreak
        )
        plugin = config["plugins"]["configs"]["jailbreak-inject"]
        self.assertTrue(plugin["enabled"])
        self.assertEqual(plugin["priority"], 2)
        self.assertEqual(plugin["suffix"], "-jailbreak")
        self.assertEqual(plugin["profiles"][0]["prompt"], "JB")
        self.assertNotIn("opencode-go-quota", config["plugins"]["configs"])

        models = config["api-keys"]["openai-compatibility"][0]["models"]
        aliases = [item.get("alias") for item in models]
        self.assertIn("deepseek-v4.1-flash", aliases)
        self.assertIn("deepseek-v4.1-flash-jailbreak", aliases)
        self.assertNotIn("oauth-model-alias", config)

    def test_devin_alias_uses_fork(self):
        jailbreak = {
            "suffix": "-jailbreak",
            "unmapped": "error",
            "profiles": [
                {
                    "name": "gemini",
                    "models": ["devin/gemini-4-argon"],
                    "mode": "prepend",
                    "prompt": "JB",
                }
            ],
        }
        config = poolctl._build_cliproxy_config(
            self._env(), "pro-only", [], [], [], {"enabled": False}, jailbreak
        )
        aliases = config["oauth-model-alias"]["devin"]
        self.assertEqual(
            aliases,
            [
                {
                    "name": "devin/gemini-4-argon",
                    "alias": "devin/gemini-4-argon-jailbreak",
                    "fork": True,
                }
            ],
        )

    def test_alias_matching_is_case_insensitive_and_glob_aware(self):
        profiles = [{"models": ["DeepSeek-*"]}]
        self.assertTrue(poolctl._jailbreak_model_matches("deepseek-v4.1-flash", profiles))
        self.assertFalse(poolctl._jailbreak_model_matches("devin/swe-2", profiles))

    def test_alias_names_are_deduped(self):
        jailbreak = {
            "suffix": "-jailbreak",
            "profiles": [
                {"models": ["deepseek-v4.1-flash", "devin/gemini-4-argon"]}
            ],
        }
        self.assertEqual(
            poolctl._jailbreak_alias_names(jailbreak),
            ["deepseek-v4.1-flash-jailbreak", "devin/gemini-4-argon-jailbreak"],
        )

    def test_without_jailbreak_no_plugin_section(self):
        config = poolctl._build_cliproxy_config(
            self._env(), "pro-only", [], [], [], {"enabled": False}, None
        )
        self.assertNotIn("plugins", config)


VALID_ENV = """\
GATEWAY_HOST=127.0.0.1
GATEWAY_PORT=8317
GATEWAY_INTERNAL_PORT=18317
MASTER_API_KEY=sk-master-0123456789abcdef0123456789abcdef
CPA_ADMIN_SECRET=admin-0123456789abcdef0123456789abcdef
OC2API_SERVER_KEY=sk-oc2api-0123456789abcdef0123456789abcdef
OC2API_WEBUI_PASSWORD=webui-0123456789abcdef0123456789abcdef
"""

VALID_ACCOUNTS = """\
devin_pool:
  accounts:
    - id: prod1
      email: prod1@example.com
      plan: Pro
      session_token: devin-session-token$test
opencode_go_pool:
  keys: []
"""


def write_repo(root, accounts=VALID_ACCOUNTS, env=VALID_ENV):
    root = Path(root)
    (root / "accounts.yaml").write_text(accounts, encoding="utf-8")
    (root / ".env").write_text(env, encoding="utf-8")


class RenderSafetyTests(unittest.TestCase):
    def _render(self, root):
        with mock.patch.object(poolctl, "_sync_opencode2api_container"), \
                mock.patch.object(poolctl, "_check_plugin_reload"), \
                contextlib.redirect_stdout(io.StringIO()):
            return poolctl.render(root)

    def test_render_keeps_sensitive_outputs_private(self):
        with tempfile.TemporaryDirectory() as tmp:
            root = Path(tmp)
            write_repo(root)
            self.assertEqual(self._render(root), 0)

            paths = [
                root / "cliproxy" / "config.yaml",
                root / "cliproxy" / "auths" / "prod1.json",
                root / "opencode2api" / "config.json",
            ]
            for path in paths:
                self.assertEqual(
                    stat.S_IMODE(path.stat().st_mode),
                    0o600,
                    path,
                )
            self.assertEqual(
                stat.S_IMODE((root / "cliproxy" / "auths").stat().st_mode),
                0o700,
            )

    def test_render_removes_stale_devin_credentials_with_custom_ids(self):
        with tempfile.TemporaryDirectory() as tmp:
            root = Path(tmp)
            write_repo(root)
            auths = root / "cliproxy" / "auths"
            auths.mkdir(parents=True)
            (auths / "custom.json").write_text(
                json.dumps({"type": "devin", "api_key": "old"}),
                encoding="utf-8",
            )
            (auths / "devin-old.json").write_text(
                json.dumps({"type": "devin", "api_key": "old"}),
                encoding="utf-8",
            )
            (auths / "unrelated.json").write_text(
                json.dumps({"type": "other", "api_key": "keep"}),
                encoding="utf-8",
            )

            self.assertEqual(self._render(root), 0)

            self.assertFalse((auths / "custom.json").exists())
            self.assertFalse((auths / "devin-old.json").exists())
            self.assertTrue((auths / "unrelated.json").exists())


class ConfigSafetyTests(unittest.TestCase):
    def test_placeholder_secrets_are_reported(self):
        env = poolctl.parse_env_file(Path(__file__).resolve().parent.parent / ".env.example")
        owners = {owner for owner, _reason in poolctl._secret_problems(env)}
        for key in (
            "MASTER_API_KEY",
            "CPA_ADMIN_SECRET",
            "OC2API_SERVER_KEY",
            "OC2API_WEBUI_PASSWORD",
        ):
            self.assertIn(key, owners)

    def test_invalid_ports_are_user_errors(self):
        with self.assertRaises(poolctl.PoolctlError):
            poolctl._build_nginx_conf(
                {"GATEWAY_PORT": "8317", "GATEWAY_INTERNAL_PORT": "not-a-port"}
            )
        with self.assertRaises(poolctl.PoolctlError):
            poolctl._gateway({"GATEWAY_HOST": "127.0.0.1", "GATEWAY_PORT": "70000"})

    def test_generated_config_keeps_template_metadata(self):
        config = poolctl._build_cliproxy_config(
            CliproxyPluginConfigTests()._env(),
            "pro-only",
            [],
            [],
            [],
            {"enabled": False},
            None,
        )
        settings = config["oauth"]["settings"]["devin"]
        self.assertEqual(settings[0]["max-context-length"], 262144)
        models = config["api-keys"]["openai-compatibility"][0]["models"]
        self.assertEqual(models[0]["max-context-length"], 1048576)
        self.assertEqual(
            models[0]["thinking"]["levels"],
            ["none", "low", "high", "max"],
        )


class RotateSecretTests(unittest.TestCase):
    def test_rotate_server_key_updates_env_privately(self):
        with tempfile.TemporaryDirectory() as tmp:
            root = Path(tmp)
            write_repo(root)
            before = poolctl.parse_env_file(root / ".env")["OC2API_SERVER_KEY"]
            with contextlib.redirect_stdout(io.StringIO()):
                self.assertEqual(poolctl.rotate_secret(root, "server"), 0)
            after = poolctl.parse_env_file(root / ".env")["OC2API_SERVER_KEY"]
            self.assertNotEqual(after, before)
            self.assertEqual(stat.S_IMODE((root / ".env").stat().st_mode), 0o600)


class SwitchModeTests(unittest.TestCase):
    def test_switch_mode_keeps_accounts_file_private(self):
        with tempfile.TemporaryDirectory() as tmp:
            root = Path(tmp)
            write_repo(root)
            with mock.patch.object(poolctl, "render", return_value=0), \
                    contextlib.redirect_stdout(io.StringIO()):
                self.assertEqual(poolctl.switch_mode(root, "max-hybrid", force=True), 0)
            self.assertEqual(
                stat.S_IMODE((root / "accounts.yaml").stat().st_mode),
                0o600,
            )


class QuotaCommandTests(unittest.TestCase):
    def test_quota_returns_nonzero_when_management_is_unreachable(self):
        with tempfile.TemporaryDirectory() as tmp:
            root = Path(tmp)
            write_repo(root, accounts="""\
devin_pool:
  accounts: []
opencode_go_pool:
  keys: []
""")
            with mock.patch.object(
                poolctl, "_management_get", return_value=(0, "network down")
            ), contextlib.redirect_stdout(io.StringIO()):
                self.assertEqual(poolctl.quota(root), 1)

    def test_quota_expands_plugin_groups(self):
        with tempfile.TemporaryDirectory() as tmp:
            root = Path(tmp)
            write_repo(root, accounts="""\
devin_pool:
  accounts: []
opencode_go_pool:
  keys:
    - key-aaaaaaaa
""")
            bucket = [
                {"window": "5h", "remainingFraction": 0.8},
                {"window": "weekly", "remainingFraction": 0.6},
                {"window": "monthly", "remainingFraction": 0.4},
            ]
            output = io.StringIO()
            with mock.patch.object(
                poolctl,
                "_management_get",
                side_effect=[
                    (
                        200,
                        {
                            "data": {
                                "files": [
                                    {
                                        "provider": "opencode-go",
                                        "name": "opencode-go-pool.json",
                                        "auth_index": "idx",
                                    }
                                ]
                            }
                        },
                    ),
                ],
            ), mock.patch.object(
                poolctl,
                "_http_json",
                return_value=(
                    200,
                    {
                        "groups": [
                            {"displayName": "key-…aaaa", "buckets": bucket},
                            {"displayName": "所有 key 综合", "buckets": bucket},
                        ]
                    },
                ),
            ), contextlib.redirect_stdout(output):
                self.assertEqual(poolctl.quota(root), 0)
            text = output.getvalue()
            self.assertIn("key-…aaaa", text)
            self.assertIn("所有 key 综合", text)


if __name__ == "__main__":
    unittest.main()
