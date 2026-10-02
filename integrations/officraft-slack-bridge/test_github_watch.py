"""最小可跑檢查:渲染、行數上限、查詢組裝。跑法:uv run python test_github_watch.py"""
import sys
import github_watch as gw

fails=[]
def check(n,c):
    print(("PASS " if c else "FAIL ")+n)
    if not c: fails.append(n)

item = {"number":396,"title":"feat(ci): example pull request [ABC-123]",
        "user":{"login":"octocat"},
        "html_url":"https://github.com/example-org/example-repo/pull/396"}

out = gw.render([("個人指名", item)])
check("標題含筆數", out.startswith("[GitHub] 1 筆新的 review request"))
check("含 repo 與 PR 編號", "example-org/example-repo #396" in out)
check("含作者與原因", "octocat" in out and "個人指名" in out)
check("含連結", item["html_url"] in out)

# 超過上限要說明被藏了幾筆,而不是靜默截斷
many = [("個人指名", dict(item, number=i, html_url=f"https://github.com/o/r/pull/{i}")) for i in range(30)]
out2 = gw.render(many)
check("超量時明講還有幾筆", f"另有 {30-gw.MAX_LINES} 筆未列出" in out2)
check("列出的筆數不超過上限", out2.count("• ") == gw.MAX_LINES)
check("聊天內容不超過 4000 字上限", len(out2) < 4000)

cfg = {"github":{"teams":["org/a","org/b"],"exclude_authors":["app/renovate"]}}
qs = gw.queries(cfg)
check("個人 + 每個 team 各一條查詢", len(qs) == 3)
check("個人查詢用 review-requested:@me", "review-requested:@me" in qs[0][1])
check("team 查詢用 team-review-requested", "team-review-requested:org/a" in qs[1][1])
check("排除作者有帶進查詢", all("-author:app/renovate" in q for _,q in qs))

print("\n" + ("全部通過" if not fails else f"失敗 {len(fails)} 項: {fails}"))
sys.exit(1 if fails else 0)
