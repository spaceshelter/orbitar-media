#!/usr/bin/env python3
"""Differential test: upload the synth corpus to the PHP (:8443) and Go (:8445) stacks, then
compare every URL shape. Usage: diff.py <corpus dir> <work dir>"""
import json, os, subprocess, sys, time, hashlib

CLIENT_ID = os.environ["CLIENT_ID"]
PORTS = {"php": 8443, "go": 8445}
corpus, work = sys.argv[1], sys.argv[2]
os.makedirs(work, exist_ok=True)

def curl(side, path, *extra, out=None, method=None):
    port = PORTS[side]
    hdr = os.path.join(work, "hdr")
    cmd = ["curl", "-sk", "--max-time", "600", "--resolve", f"orbitar.media.local:{port}:127.0.0.1",
           "-D", hdr, "-o", out or os.path.join(work, "body")]
    if method: cmd += ["-X", method]
    cmd += list(extra) + [f"https://orbitar.media.local:{port}{path}"]
    subprocess.run(cmd, check=False)
    lines = open(hdr, encoding="latin1").read().splitlines()
    # last response block (redirects are not followed)
    blocks = [i for i, l in enumerate(lines) if l.startswith("HTTP/")]
    status = int(lines[blocks[-1]].split()[1])
    h = {}
    for l in lines[blocks[-1] + 1:]:
        if ":" in l:
            k, v = l.split(":", 1)
            h[k.strip().lower()] = v.strip()
    body = open(out or os.path.join(work, "body"), "rb").read()
    return status, h, body

def upload(side, f):
    s, h, b = curl(side, "/api/upload.php", "-H", f"Authorization: Client-ID {CLIENT_ID}", "-F", f"file=@{f}")
    try:
        return json.loads(b)
    except ValueError:
        return {"_status": s, "_body": b[:200].decode("latin1")}

def norm_upload(j):
    j = dict(j)
    for k in ("hash", "url", "delete_code", "delete_url"):
        if k in j: j[k] = "<%s>" % k
    return j

def probe(path):
    r = subprocess.run(["ffprobe", "-v", "error", "-show_entries",
                        "stream=codec_type,codec_name,profile,pix_fmt,width,height,field_order,channels:format=nb_streams",
                        "-of", "json", path], capture_output=True, text=True)
    try:
        d = json.loads(r.stdout)
    except ValueError:
        return None
    with open(path, "rb") as fh:
        head = fh.read(64 * 1024 * 1024)
    moov, mdat = head.find(b"moov"), head.find(b"mdat")
    d["moov_first"] = moov != -1 and (mdat == -1 or moov < mdat)
    return d

def img_info(path):
    r = subprocess.run(["magick", "identify", "-format", "%m %w %h|", path], capture_output=True, text=True)
    return r.stdout.split("|")[0] if r.returncode == 0 else None

def rmse(a, b):
    r = subprocess.run(["magick", "compare", "-metric", "RMSE", a + "[0]", b + "[0]", "null:"], capture_output=True, text=True)
    try:
        return float(r.stderr.split("(")[1].split(")")[0])
    except (IndexError, ValueError):
        return None

problems, notes = [], []
def report(kind, msg):
    (problems if kind == "FAIL" else notes).append(msg)
    print(f"{kind:5} {msg}", flush=True)

# 1. uploads
files = sorted(os.listdir(corpus))
pairs = {}
for f in files:
    p = os.path.join(corpus, f)
    a, b = upload("php", p), upload("go", p)
    na, nb = norm_upload(a), norm_upload(b)
    if na != nb:
        report("DIFF", f"upload {f}: php={json.dumps(na)} go={json.dumps(nb)}")
    else:
        print(f"ok    upload {f}: {json.dumps(na)}")
    if a.get("status") == "ok" and b.get("status") == "ok":
        pairs[f] = (a["hash"], b["hash"])
    elif a.get("status") == "ok" or b.get("status") == "ok":
        report("FAIL", f"upload {f}: only one side accepted it")

# dedupe on both
for f in ("i_rgba.png", "v_ok.mp4"):
    if f in pairs:
        p = os.path.join(corpus, f)
        for side, idx in (("php", 0), ("go", 1)):
            j = upload(side, p)
            if j.get("hash") != pairs[f][idx] or "delete_code" in j:
                report("FAIL", f"dedupe {f} on {side}: {j}")

# 2. let background transcodes finish
for _ in range(240):
    busy = 0
    for c in ("omdiff-pictshare-1", "omdiff-mediaserver-1"):
        r = subprocess.run(["docker", "exec", c, "sh", "-c", "pgrep -x ffmpeg | wc -l"], capture_output=True, text=True)
        busy += int(r.stdout.strip() or 0)
    if not busy: break
    time.sleep(5)

# 3. URL shapes
def shapes(name):
    ext = name.rsplit(".", 1)[1]
    if ext == "mp4":
        return ["/{h}", "/raw/{h}", "/{h}/raw", "/preview/{h}", "/preview/320/{h}", "/320/preview/{h}", "/download/{h}", "/300/{h}", "/preview/5000/{h}"]
    if ext == "gif":
        return ["/{h}", "/300/{h}", "/mp4/{h}", "/mp4/raw/{h}", "/mp4/preview/{h}"]
    return ["/{h}", "/300/{h}", "/w/300/{h}", "/50x50/{h}", "/left/{h}", "/300/right/{h}", "/upside/{h}",
            "/200x200/forcesize/{h}", "/300/webp/{h}", "/sepia/300/{h}", "/{h}/300", "/download/{h}"]

CMP_HEADERS = ("content-type", "cache-control", "content-disposition")
for f, (hp, hg) in sorted(pairs.items()):
    for shape in shapes(hp):
        pa, pg = os.path.join(work, "a"), os.path.join(work, "b")
        sa, ha, ba = curl("php", shape.format(h=hp), out=pa)
        sg, hgd, bg = curl("go", shape.format(h=hg), out=pg)
        label = f"{f} {shape}"
        if sa != sg:
            report("FAIL", f"{label}: status php={sa} go={sg} (php {ba[:80]!r} / go {bg[:80]!r})")
            continue
        for k in CMP_HEADERS:
            va, vg = ha.get(k, ""), hgd.get(k, "")
            if k == "content-disposition":
                va, vg = va.replace(hp, "<h>"), vg.replace(hg, "<h>")
            if k == "content-type":
                va, vg = va.lower().replace(" ", ""), vg.lower().replace(" ", "")
            if va != vg:
                report("DIFF", f"{label}: {k} php={va!r} go={vg!r}")
        if sa >= 400:
            continue
        if sa == 200 and "/download/" not in shape and (not ha.get("etag") or not hgd.get("etag")) and "html" not in ha.get("content-type", ""):
            report("DIFF", f"{label}: etag php={ha.get('etag')!r} go={hgd.get('etag')!r}")
        ct = ha.get("content-type", "")
        if ct.startswith("image/"):
            ia, ig = img_info(pa), img_info(pg)
            if ia != ig:
                report("FAIL", f"{label}: image php={ia} go={ig}")
            elif "/sepia/" in shape:
                pass  # colour filters are dropped on purpose: same size, no filter
            else:
                e = rmse(pa, pg)
                if e is None or e > 0.03:
                    report("FAIL" if e is None or e > 0.08 else "DIFF", f"{label}: {ia} rmse={e}")
        elif ct.startswith("video/") or "octet-stream" in ct:
            if f.endswith(".mp4") or "/mp4/" in shape:
                qa, qg = probe(pa), probe(pg)
                if qa != qg:
                    report("FAIL", f"{label}: probe php={json.dumps(qa)} go={json.dumps(qg)}")
                if ba == bg:
                    pass
                elif shape.startswith("/raw") or shape.endswith("/raw"):
                    report("NOTE", f"{label}: bytes differ ({len(ba)} vs {len(bg)}), same probe")
            elif ba != bg:
                report("FAIL", f"{label}: download bytes differ")
        elif "html" in ct:
            if f.endswith(".mp4") and b"<video" not in bg:
                report("FAIL", f"{label}: no <video> in the go page")

    # Range on video raw
    if hp.endswith(".mp4"):
        sa, ha, ba = curl("php", f"/raw/{hp}", "-H", "Range: bytes=0-99")
        sg, hgd, bg = curl("go", f"/raw/{hg}", "-H", "Range: bytes=0-99")
        if (sa, len(ba)) != (sg, len(bg)):
            report("FAIL", f"{f} range: php={sa}/{len(ba)} go={sg}/{len(bg)}")

# 4. API odds and ends
for path, extra in [("/api/info.php?hash={h}", True), ("/api/upload.php", False), ("/api/info.php?hash=nope.jpg", True), ("/delete_wrong/{h}", False), ("/nothere.jpg", False)]:
    f = "i_rgba.png"
    if f not in pairs: break
    hp, hg = pairs[f]
    a = curl("php", path.format(h=hp), *(["-H", f"Authorization: Client-ID {CLIENT_ID}"] if extra else []))
    b = curl("go", path.format(h=hg), *(["-H", f"Authorization: Client-ID {CLIENT_ID}"] if extra else []))
    ba, bb = a[2].replace(hp.encode(), b"<h>"), b[2].replace(hg.encode(), b"<h>")
    if a[0] != b[0] or ba != bb:
        report("DIFF", f"{path}: php={a[0]} {ba[:160]!r} go={b[0]} {bb[:160]!r}")

json.dump({k: list(v) for k, v in pairs.items()}, open(os.path.join(work, "pairs.json"), "w"), indent=1)
print(f"\n{len(pairs)} pairs, {len(problems)} failures, {len(notes)} diffs/notes")
