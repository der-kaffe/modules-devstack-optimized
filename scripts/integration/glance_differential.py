#!/usr/bin/env python3
"""Differential comparison of Glance Go against the original Glance, live.

Runs the same requests against both services (they share the catalog, MySQL
and store) and compares status codes and normalized JSON bodies: UUIDs are
replaced by their order of appearance, timestamps by a placeholder, and the
per-implementation suffix of created names is removed. Read-only requests
compare the same resources; mutating scenarios use distinct names per
implementation and are cleaned up.

Usage (on the DevStack VM, as stack, with openrc admin admin sourced):
  glance_differential.py --original http://127.0.0.1:60999 \
      --go http://127.0.0.1:19292 --output DIR
Exit status 1 when any difference remains. Tokens never reach the output.
"""
import argparse
import json
import os
import re
import subprocess
import sys
import time
import urllib.error
import urllib.request

UUID = re.compile(r"[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}")
TS = re.compile(r"\d{4}-\d{2}-\d{2}T\d{2}:\d{2}:\d{2}(\.\d+)?Z?")


def token(project, user="admin"):
    env = dict(os.environ, OS_PROJECT_NAME=project, OS_USERNAME=user)
    return subprocess.check_output(["openstack", "token", "issue", "-f", "value", "-c", "id"], env=env, text=True).strip()


def call(base, tok, method, path, body=None, media="application/json", headers=None):
    data = None
    if body is not None:
        data = body if isinstance(body, bytes) else json.dumps(body).encode()
    req = urllib.request.Request(base + path, data=data, method=method)
    req.add_header("X-Auth-Token", tok)
    req.add_header("Accept", "application/json")
    if data is not None:
        req.add_header("Content-Type", media)
    for k, v in (headers or {}).items():
        req.add_header(k, v)
    try:
        with urllib.request.urlopen(req, timeout=60) as r:
            status, raw = r.status, r.read()
    except urllib.error.HTTPError as e:
        status, raw = e.code, e.read()
    try:
        out = json.loads(raw) if raw else None
    except ValueError:
        out = "<non-json>"
    return status, out


class Normalizer:
    def __init__(self, suffix):
        self.ids, self.suffix = {}, suffix

    def value(self, v):
        if isinstance(v, dict):
            return {self.text(k): self.value(x) for k, x in v.items()}
        if isinstance(v, list):
            return [self.value(x) for x in v]
        if isinstance(v, str):
            return self.text(v)
        return v

    def text(self, s):
        s = s.replace(self.suffix, "<sfx>")
        s = TS.sub("<ts>", s)
        return UUID.sub(lambda m: self.ids.setdefault(m.group(0), "<id%d>" % len(self.ids)), s)


IGNORED = {"<br /><br />\n\n\n"}  # body detail of error pages


def strip(v, ignore):
    if isinstance(v, dict):
        return {k: strip(x, ignore) for k, x in v.items() if k not in ignore}
    if isinstance(v, list):
        return [strip(x, ignore) for x in v]
    return v


class Diff:
    def __init__(self, out):
        self.out, self.cases, self.diffs = out, 0, []

    def compare(self, name, a, b, ignore=()):
        self.cases += 1
        a, b = (a[0], strip(a[1], set(ignore))), (b[0], strip(b[1], set(ignore)))
        if a != b:
            self.diffs.append({"case": name, "original": a, "go": b})


def scenario(base, admin, member, sfx, image_url):
    """One implementation's mutating scenario; returns normalized results."""
    n = Normalizer(sfx)
    results = []

    def rec(label, r):
        results.append((label, (r[0], n.value(r[1]))))
        return r

    # Images: shared image, members, tags by path, actions.
    s, img = rec("create shared image", call(base, admin, "POST", "/v2/images", {"name": "diff" + sfx, "visibility": "shared", "disk_format": "raw", "container_format": "bare"}))
    iid = img["id"]
    rec("add member", call(base, admin, "POST", "/v2/images/%s/members" % iid, {"member": member[1]}))
    rec("add member twice", call(base, admin, "POST", "/v2/images/%s/members" % iid, {"member": member[1]}))
    rec("list members", call(base, admin, "GET", "/v2/images/%s/members" % iid))
    rec("member sees image", call(base, member[0], "GET", "/v2/images/%s" % iid))
    rec("owner accepts for member", call(base, admin, "PUT", "/v2/images/%s/members/%s" % (iid, member[1]), {"status": "accepted"}))
    rec("member accepts", call(base, member[0], "PUT", "/v2/images/%s/members/%s" % (iid, member[1]), {"status": "accepted"}))
    rec("bad member status", call(base, member[0], "PUT", "/v2/images/%s/members/%s" % (iid, member[1]), {"status": "maybe"}))
    rec("get member", call(base, member[0], "GET", "/v2/images/%s/members/%s" % (iid, member[1])))
    rec("member modifies", call(base, member[0], "PATCH", "/v2/images/%s" % iid, [{"op": "replace", "path": "/name", "value": "x"}], "application/openstack-images-v2.1-json-patch"))
    rec("tag add", call(base, admin, "PUT", "/v2/images/%s/tags/t1" % iid))
    rec("tag add again", call(base, admin, "PUT", "/v2/images/%s/tags/t1" % iid))
    rec("tag delete", call(base, admin, "DELETE", "/v2/images/%s/tags/t1" % iid))
    rec("tag delete missing", call(base, admin, "DELETE", "/v2/images/%s/tags/t1" % iid))
    rec("deactivate queued", call(base, admin, "POST", "/v2/images/%s/actions/deactivate" % iid))
    rec("upload", call(base, admin, "PUT", "/v2/images/%s/file" % iid, b"0123456789" * 100, "application/octet-stream"))
    rec("deactivate", call(base, admin, "POST", "/v2/images/%s/actions/deactivate" % iid))
    rec("deactivate again", call(base, admin, "POST", "/v2/images/%s/actions/deactivate" % iid))
    rec("member downloads deactivated", call(base, member[0], "GET", "/v2/images/%s/file" % iid))
    rec("reactivate", call(base, admin, "POST", "/v2/images/%s/actions/reactivate" % iid))
    rec("show", call(base, admin, "GET", "/v2/images/%s" % iid))
    rec("remove member", call(base, admin, "DELETE", "/v2/images/%s/members/%s" % (iid, member[1])))
    rec("delete image", call(base, admin, "DELETE", "/v2/images/%s" % iid))

    # Interoperable import: stage + glance-direct, web-download, tasks.
    s, img = rec("create for import", call(base, admin, "POST", "/v2/images", {"name": "imp" + sfx, "disk_format": "raw", "container_format": "bare"}))
    iid = img["id"]
    rec("glance-direct unstaged", call(base, admin, "POST", "/v2/images/%s/import" % iid, {"method": {"name": "glance-direct"}}))
    rec("stage", call(base, admin, "PUT", "/v2/images/%s/stage" % iid, b"staged" * 100, "application/octet-stream"))
    rec("staged status", call(base, admin, "GET", "/v2/images/%s" % iid))
    rec("import glance-direct", call(base, admin, "POST", "/v2/images/%s/import" % iid, {"method": {"name": "glance-direct"}}))
    for _ in range(100):
        s, i = call(base, admin, "GET", "/v2/images/%s" % iid)
        if i.get("status") == "active" and "os_glance_import_task" not in i:
            break
        time.sleep(0.2)
    rec("imported", (s, i))
    s, tasks = rec("image tasks", call(base, admin, "GET", "/v2/images/%s/tasks" % iid))
    if tasks and tasks.get("tasks"):
        rec("task", call(base, admin, "GET", "/v2/tasks/%s" % tasks["tasks"][0]["id"]))
    rec("delete imported", call(base, admin, "DELETE", "/v2/images/%s" % iid))
    s, img = call(base, admin, "POST", "/v2/images", {"name": "web" + sfx, "disk_format": "raw", "container_format": "bare"})
    iid = img["id"]
    rec("web-download bad uri", call(base, admin, "POST", "/v2/images/%s/import" % iid, {"method": {"name": "web-download", "uri": "ftp://example.org/x"}}))
    rec("web-download", call(base, admin, "POST", "/v2/images/%s/import" % iid, {"method": {"name": "web-download", "uri": image_url}}))
    for _ in range(100):
        s, i = call(base, admin, "GET", "/v2/images/%s" % iid)
        if i.get("status") in ("active", "queued") and "os_glance_import_task" not in i:
            break
        time.sleep(0.2)
    rec("web-downloaded", (s, i))
    rec("delete web", call(base, admin, "DELETE", "/v2/images/%s" % iid))

    # Metadefs CRUD.
    ns = "OS::Diff::NS" + sfx
    body = {"namespace": ns, "display_name": "Diff", "description": "d", "visibility": "public", "protected": False,
            "properties": {"p1": {"title": "P1", "type": "string"}},
            "objects": [{"name": "o1", "properties": {"x": {"title": "X", "type": "integer"}}, "required": ["x"]}],
            "tags": [{"name": "tag1"}], "resource_type_associations": [{"name": "OS::Glance::Image", "prefix": "diff_"}]}
    rec("ns create", call(base, admin, "POST", "/v2/metadefs/namespaces", body))
    rec("ns create twice", call(base, admin, "POST", "/v2/metadefs/namespaces", body))
    rec("ns get", call(base, admin, "GET", "/v2/metadefs/namespaces/" + ns))
    rec("ns get rt", call(base, admin, "GET", "/v2/metadefs/namespaces/%s?resource_type=OS::Glance::Image" % ns))
    rec("ns member create", call(base, member[0], "POST", "/v2/metadefs/namespaces", dict(body, namespace=ns + "m")))
    rec("prop create", call(base, admin, "POST", "/v2/metadefs/namespaces/%s/properties" % ns, {"name": "p2", "title": "P2", "type": "boolean"}))
    rec("prop invalid", call(base, admin, "POST", "/v2/metadefs/namespaces/%s/properties" % ns, {"name": "p3", "type": "boolean"}))
    rec("prop get", call(base, admin, "GET", "/v2/metadefs/namespaces/%s/properties/p2" % ns))
    rec("prop get prefixed", call(base, admin, "GET", "/v2/metadefs/namespaces/%s/properties/diff_p2" % ns))
    rec("prop update", call(base, admin, "PUT", "/v2/metadefs/namespaces/%s/properties/p2" % ns, {"name": "p2", "title": "P2b", "type": "boolean"}))
    rec("props", call(base, admin, "GET", "/v2/metadefs/namespaces/%s/properties" % ns))
    rec("obj create", call(base, admin, "POST", "/v2/metadefs/namespaces/%s/objects" % ns, {"name": "o2", "description": "o"}))
    rec("obj update", call(base, admin, "PUT", "/v2/metadefs/namespaces/%s/objects/o2" % ns, {"name": "o2", "description": "o2"}))
    rec("obj get", call(base, admin, "GET", "/v2/metadefs/namespaces/%s/objects/o2" % ns))
    rec("objs", call(base, admin, "GET", "/v2/metadefs/namespaces/%s/objects" % ns))
    rec("obj missing", call(base, admin, "GET", "/v2/metadefs/namespaces/%s/objects/none" % ns))
    rec("tag create", call(base, admin, "POST", "/v2/metadefs/namespaces/%s/tags/tag2" % ns))
    rec("tag dup", call(base, admin, "POST", "/v2/metadefs/namespaces/%s/tags/TAG2" % ns))
    rec("tags append", call(base, admin, "POST", "/v2/metadefs/namespaces/%s/tags" % ns, {"tags": [{"name": "tag3"}]}, headers={"X-Openstack-Append": "True"}))
    rec("tag rename", call(base, admin, "PUT", "/v2/metadefs/namespaces/%s/tags/tag3" % ns, {"name": "tag4"}))
    rec("tags", call(base, admin, "GET", "/v2/metadefs/namespaces/%s/tags" % ns))
    rec("rt associate", call(base, admin, "POST", "/v2/metadefs/namespaces/%s/resource_types" % ns, {"name": "OS::Nova::Flavor"}))
    rec("rt list", call(base, admin, "GET", "/v2/metadefs/namespaces/%s/resource_types" % ns))
    rec("rt dissociate", call(base, admin, "DELETE", "/v2/metadefs/namespaces/%s/resource_types/OS::Nova::Flavor" % ns))
    rec("ns update", call(base, admin, "PUT", "/v2/metadefs/namespaces/" + ns, {"namespace": ns, "display_name": "Diff2", "visibility": "public", "protected": True}))
    rec("protected delete", call(base, admin, "DELETE", "/v2/metadefs/namespaces/" + ns))
    rec("protected obj delete", call(base, admin, "DELETE", "/v2/metadefs/namespaces/%s/objects/o2" % ns))
    rec("unprotect", call(base, admin, "PUT", "/v2/metadefs/namespaces/" + ns, {"namespace": ns, "protected": False}))
    rec("obj delete", call(base, admin, "DELETE", "/v2/metadefs/namespaces/%s/objects/o2" % ns))
    rec("props delete all", call(base, admin, "DELETE", "/v2/metadefs/namespaces/%s/properties" % ns))
    rec("ns delete", call(base, admin, "DELETE", "/v2/metadefs/namespaces/" + ns))
    rec("ns gone", call(base, admin, "GET", "/v2/metadefs/namespaces/" + ns))
    return results


def main():
    p = argparse.ArgumentParser()
    p.add_argument("--original", required=True)
    p.add_argument("--go", required=True)
    p.add_argument("--output", required=True)
    p.add_argument("--image-url", default="http://192.168.122.10/identity/v3")
    a = p.parse_args()
    os.makedirs(a.output, exist_ok=False)
    admin = token("admin")
    member = (token("demo", "demo"), subprocess.check_output(["openstack", "project", "show", "demo", "-f", "value", "-c", "id"], text=True).strip())
    d = Diff(a.output)
    # Read-only: the same resources from both.
    reads = ["/v2/schemas/" + s for s in ["image", "images", "member", "members", "task", "tasks", "metadefs/namespace", "metadefs/namespaces",
                                          "metadefs/object", "metadefs/objects", "metadefs/property", "metadefs/properties", "metadefs/tag",
                                          "metadefs/tags", "metadefs/resource_type", "metadefs/resource_types"]]
    reads += ["/v2/info/import", "/v2/info/stores", "/v2/info/usage", "/v2/cache", "/v2/metadefs/resource_types",
              "/v2/metadefs/namespaces", "/v2/metadefs/namespaces?limit=3", "/v2/metadefs/namespaces?sort_key=namespace&sort_dir=asc&limit=5",
              "/v2/metadefs/namespaces?resource_types=OS::Glance::Image", "/v2/metadefs/namespaces?visibility=public&limit=2",
              "/v2/metadefs/namespaces/nope", "/v2/metadefs/namespaces/OS::Compute::Libvirt/properties/nope", "/v2/tasks?limit=1",
              "/v2/images?member_status=pending", "/v2/images?visibility=shared"]
    s, nss = call(a.original, admin, "GET", "/v2/metadefs/namespaces?limit=100")
    for ns in nss.get("namespaces", []):
        name = ns["namespace"]
        base = "/v2/metadefs/namespaces/" + name
        reads += [base, base + "/objects", base + "/properties", base + "/tags", base + "/resource_types"]
    for path in reads:
        for tok, who in ((admin, "admin"), (member[0], "member")):
            d.compare("GET %s as %s" % (path, who), call(a.original, tok, "GET", path), call(a.go, tok, "GET", path))
    py = scenario(a.original, admin, member, "-py", a.image_url)
    go = scenario(a.go, admin, member, "-go", a.image_url)
    for (label, r1), (_, r2) in zip(py, go):
        d.compare(label, r1, r2)
    report = {"cases": d.cases, "differences": len(d.diffs), "diffs": d.diffs}
    with open(os.path.join(a.output, "differential.json"), "w") as f:
        json.dump(report, f, indent=1, sort_keys=True)
    print(json.dumps({"cases": d.cases, "differences": len(d.diffs)}))
    return 1 if d.diffs else 0


if __name__ == "__main__":
    sys.exit(main())
