import importlib.util
import tempfile
import unittest
from pathlib import Path


def load_poolctl():
    path = Path(__file__).resolve().parent.parent / "scripts" / "poolctl.py"
    spec = importlib.util.spec_from_file_location("poolctl", path)
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


if __name__ == "__main__":
    unittest.main()
