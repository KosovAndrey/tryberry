#!/usr/bin/env python3
"""Google Tasks helper для трекера задач проекта (список «tryberrybot»).

Креды — в локальном .env (в корне репо, gitignored): GOOGLE_CLIENT_ID,
GOOGLE_CLIENT_SECRET, GOOGLE_REFRESH_TOKEN. Секреты в логи/git НЕ пишем.

Режимы:
  list            — показать незавершённые задачи (для хука/обзора).
  add "<текст>" [due] [prio] [--notes "<описание>"]
                  — добавить задачу (due=YYYY-MM-DD опц., prio=🔴/🟡/🟢 опц.).
                    Лимиты API: title 1024, notes 8192 — при перерасходе голый 400.
  done "<подстрока>"          — пометить выполненной задачу, чьё название содержит подстроку.
  url | exchange | fill       — разовая OAuth-настройка/первичная заливка (см. историю).

Подробности и грабли — память notion-kanban.md.
"""
import sys, os, json, time, datetime, http.server, urllib.parse, urllib.request

ROOT = os.path.dirname(os.path.dirname(os.path.abspath(__file__)))
ENV = os.path.join(ROOT, ".env")
PORT = 8765
REDIRECT = f"http://localhost:{PORT}"
SCOPE = "https://www.googleapis.com/auth/tasks"
LIST_NAME = "tryberrybot"


def env_get(k):
    try:
        for line in open(ENV):
            if line.startswith(k + "="):
                return line.split("=", 1)[1].strip().strip('"').strip("'")
    except FileNotFoundError:
        pass
    return None


def env_set(k, v):
    lines = open(ENV).read().splitlines() if os.path.exists(ENV) else []
    for i, line in enumerate(lines):
        if line.startswith(k + "="):
            lines[i] = f"{k}={v}"
            break
    else:
        lines.append(f"{k}={v}")
    open(ENV, "w").write("\n".join(lines) + "\n")


CID = env_get("GOOGLE_CLIENT_ID")
CSEC = env_get("GOOGLE_CLIENT_SECRET")


def access_token():
    data = urllib.parse.urlencode({
        "client_id": CID, "client_secret": CSEC,
        "refresh_token": env_get("GOOGLE_REFRESH_TOKEN"), "grant_type": "refresh_token",
    }).encode()
    tok = json.load(urllib.request.urlopen(urllib.request.Request("https://oauth2.googleapis.com/token", data=data)))
    return tok["access_token"]


def api(method, path, at, body=None, query=""):
    url = "https://tasks.googleapis.com/tasks/v1/" + path + (("?" + query) if query else "")
    data = json.dumps(body).encode() if body is not None else None
    req = urllib.request.Request(url, data=data, method=method,
                                 headers={"Authorization": "Bearer " + at, "Content-Type": "application/json"})
    return json.load(urllib.request.urlopen(req))


def list_id(at):
    for l in api("GET", "users/@me/lists", at).get("items", []):
        if l.get("title") == LIST_NAME:
            return l["id"]
    return None


def cmd_list():
    at = access_token()
    lid = list_id(at)
    if not lid:
        print(f"(список «{LIST_NAME}» не найден)")
        return
    items = api("GET", f"lists/{lid}/tasks", at, query="showCompleted=false&maxResults=100").get("items", [])
    items = [t for t in items if t.get("status") != "completed"]
    if not items:
        print(f"📋 Google Tasks «{LIST_NAME}»: активных задач нет.")
        return
    def key(t):
        return t.get("due", "9999")
    print(f"📋 Google Tasks «{LIST_NAME}» — активные ({len(items)}):")
    for t in sorted(items, key=key):
        due = (t.get("due", "")[:10]) or "—"
        print(f"  • [{due}] {t.get('title','')}")


def cmd_add(args):
    # --notes "<текст>" — необязательное описание (лимит API 8192); в заголовке
    # лимит 1024, и API отвечает голым 400 при перерасходе — поэтому длинные
    # разборы кладём в notes, а не в title.
    notes = None
    if "--notes" in args:
        i = args.index("--notes")
        notes = args[i + 1] if len(args) > i + 1 else None
        args = args[:i] + args[i + 2:]
    text = args[0]
    due = args[1] if len(args) > 1 and args[1] not in ("🔴", "🟡", "🟢") else None
    prio = next((a for a in args[1:] if a in ("🔴", "🟡", "🟢")), "")
    at = access_token()
    lid = list_id(at)
    title = (prio + " " + text).strip()
    if len(title) > 1024:
        print(f"заголовок {len(title)} симв. > 1024 — перенеси хвост в --notes")
        sys.exit(1)
    body = {"title": title}
    if notes:
        body["notes"] = notes[:8192]
    if due:
        body["due"] = due + "T00:00:00.000Z"
    api("POST", f"lists/{lid}/tasks", at, body)
    print("добавлено:", body["title"])


def cmd_done(sub):
    at = access_token()
    lid = list_id(at)
    items = api("GET", f"lists/{lid}/tasks", at, query="showCompleted=false&maxResults=100").get("items", [])
    hit = [t for t in items if sub.lower() in t.get("title", "").lower() and t.get("status") != "completed"]
    if not hit:
        print("не нашёл активную задачу с:", sub)
        return
    if len(hit) > 1:
        print("неоднозначно, совпало несколько:")
        for t in hit:
            print("  -", t.get("title"))
        return
    t = hit[0]
    api("PATCH", f"lists/{lid}/tasks/{t['id']}", at, {"status": "completed"})
    print("выполнено:", t.get("title"))


# ── разовая OAuth-настройка ───────────────────────────────────────────────────
def auth_url():
    p = {"client_id": CID, "redirect_uri": REDIRECT, "response_type": "code",
         "scope": SCOPE, "access_type": "offline", "prompt": "consent"}
    return "https://accounts.google.com/o/oauth2/v2/auth?" + urllib.parse.urlencode(p)


def exchange():
    raw = open("/tmp/gcode").read().strip()
    code = urllib.parse.parse_qs(urllib.parse.urlparse(raw).query).get("code", [raw])[0] if "code=" in raw else raw
    data = urllib.parse.urlencode({"code": code, "client_id": CID, "client_secret": CSEC,
                                   "redirect_uri": REDIRECT, "grant_type": "authorization_code"}).encode()
    tok = json.load(urllib.request.urlopen(urllib.request.Request("https://oauth2.googleapis.com/token", data=data)))
    if not tok.get("refresh_token"):
        print("НЕТ refresh_token:", json.dumps(tok)[:300]); sys.exit(1)
    env_set("GOOGLE_REFRESH_TOKEN", tok["refresh_token"])
    print("OAUTH_OK")


if __name__ == "__main__":
    cmd = sys.argv[1] if len(sys.argv) > 1 else "list"
    if cmd == "list":
        cmd_list()
    elif cmd == "add":
        cmd_add(sys.argv[2:])
    elif cmd == "done":
        cmd_done(sys.argv[2])
    elif cmd == "url":
        print(auth_url())
    elif cmd == "exchange":
        exchange()
    else:
        print("usage: list | add <текст> [YYYY-MM-DD] [🔴|🟡|🟢] | done <подстрока> | url | exchange")
