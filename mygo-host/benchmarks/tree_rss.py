import subprocess, sys

root = int(sys.argv[1])
out = subprocess.check_output(["ps", "-axo", "pid=,ppid=,rss=,comm="]).decode()
procs = {}
for line in out.splitlines():
    parts = line.split(None, 3)
    if len(parts) < 4:
        continue
    pid, ppid, rss = int(parts[0]), int(parts[1]), int(parts[2])
    procs[pid] = (ppid, rss, parts[3].strip())
children = {}
for pid, (ppid, rss, comm) in procs.items():
    children.setdefault(ppid, []).append(pid)
tree, stack = [], [root]
while stack:
    p = stack.pop()
    if p in procs and p not in tree:
        tree.append(p)
        stack.extend(children.get(p, []))
total = sum(procs[p][1] for p in tree)
print(f"{len(tree)} {total}")
for p in tree:
    if procs[p][1] > 20000:
        print(f"  {procs[p][2][:70]} {procs[p][1] // 1024}MB")
