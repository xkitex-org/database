#!/usr/bin/env python3
"""使用临时模块代理验证未发布的 database、cache、sentinel、core 和 monitor，不修改业务源码。"""

import json
import os
from pathlib import Path
import shutil
import subprocess
import tempfile
import zipfile


def snapshot(source: Path, proxy: Path, module: str) -> None:
    """将正式模块路径的源码快照写入临时代理，保留独立模块边界。"""
    version = "v0.0.1"
    target = proxy / module / "@v"
    target.mkdir(parents=True, exist_ok=True)
    (target / f"{version}.mod").write_bytes((source / "go.mod").read_bytes())
    (target / f"{version}.info").write_text(json.dumps({
        "Version": version, "Time": "2026-11-04T00:00:00Z",
    }))
    with zipfile.ZipFile(target / f"{version}.zip", "w", zipfile.ZIP_DEFLATED) as archive:
        for path in sorted(source.rglob("*")):
            relative = path.relative_to(source)
            if path.is_file() and not path.is_symlink() and not any(part.startswith(".") for part in relative.parts):
                archive.write(path, f"{module}@{version}/{relative.as_posix()}")


def main() -> None:
    """在临时副本执行测试，退出后清理代理、私有模块快照及测试数据。"""
    root = Path(__file__).resolve().parents[2]
    old_cache = Path(subprocess.check_output(["go", "env", "GOMODCACHE"], text=True).strip())
    with tempfile.TemporaryDirectory(prefix="xkitex-database-review-") as temporary:
        task = Path(temporary)
        proxy = task / "proxy"
        optional = tuple(name for name in ("cache", "sentinel") if (root / name / "go.mod").exists())
        modules = ("database",) + optional + ("core", "monitor")
        for name in modules:
            source = root / name
            if not (source / "go.mod").exists():
                raise SystemExit(f"缺少相邻独立模块：{source}")
            snapshot(source, proxy, f"go.chaoxizhongzhi.com/xkitex/{name}")
            shutil.copytree(source, task / name, ignore=shutil.ignore_patterns(".git", ".idea"))
        env = os.environ.copy()
        proxies = [proxy.as_uri()]
        if (old_cache / "cache/download").is_dir():
            proxies.append((old_cache / "cache/download").as_uri())
        proxies.append("https://proxy.golang.org")
        env.update({
            "GOWORK": "off", "GONOPROXY": "none",
            "GONOSUMDB": "go.chaoxizhongzhi.com",
            "GOMODCACHE": str(task / "modcache"),
            "GOPROXY": ",".join(proxies), "GOTOOLCHAIN": "local",
        })
        for name in modules:
            print(f"验证独立模块：{name}", flush=True)
            # 未发布快照可能与旧快照的和不同，只在临时副本重新计算该私有依赖的校验和。
            sums = task / name / "go.sum"
            if sums.exists():
                sums.write_text("".join(line for line in sums.read_text().splitlines(True)
                                        if not line.startswith("go.chaoxizhongzhi.com/xkitex/")))
            subprocess.run(["go", "test", "-mod=mod", "-race", "-timeout", "90s", "./..."],
                           cwd=task / name, env=env, check=True)
        print("、".join(modules) + " 验证通过；没有发布、没有写入 go.work 或 replace。")


if __name__ == "__main__":
    main()
