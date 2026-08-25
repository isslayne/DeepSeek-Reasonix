#!/usr/bin/env python3
from __future__ import annotations

import pathlib

TARGET = pathlib.Path("internal/agent/context_recovery_tool_loop_test.go")


def main() -> None:
    text = TARGET.read_text(encoding="utf-8")
    old = '''\terr := a.Run(context.Background(), "keep using the tool until the provider says the task is done")
\tif err == nil {
'''
    new = '''\terr := a.Run(context.Background(), "keep using the tool until the provider says the task is done")
\tprov.mu.Lock()
\tt.Logf("active-turn overflow debug: err=%v requests=%v overflows=%d summaries=%d applied=%d", err, prov.requestsAt, prov.overflows, prov.summaries, applied)
\tprov.mu.Unlock()
\tif err == nil {
'''
    if text.count(old) != 1:
        raise RuntimeError(f"expected one Run assertion, found {text.count(old)}")
    TARGET.write_text(text.replace(old, new, 1), encoding="utf-8")


if __name__ == "__main__":
    main()
