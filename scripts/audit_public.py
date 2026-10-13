"""Audit only Git-tracked publishable files; never read local secrets/state."""
import pathlib,re,subprocess,sys
root=pathlib.Path(__file__).resolve().parents[1]
files=subprocess.check_output(['git','ls-files','-z'],cwd=root).decode().split('\0')
forbidden=['.secrets/','.state/','.tools/','bin/','.local/','config.json','internal/dashboard/','cmd/greeter/','cmd/audit/','deploy/']
patterns=[r'gh[pousr]_[A-Za-z0-9]{30,}',r'github_pat_[A-Za-z0-9_]{40,}',r'-----BEGIN (?:RSA |EC |OPENSSH )?PRIVATE KEY-----',r'/Users/'+r'[A-Za-z][^\s"\']*/']
errors=[]
for name in filter(None,files):
    if any(name==x or name.startswith(x) for x in forbidden):errors.append(name+': forbidden file');continue
    data=(root/name).read_bytes()
    if b'\x00' in data:errors.append(name+': binary file');continue
    text=data.decode('utf-8')
    if name== 'scripts/audit_public.py':continue
    for pattern in patterns:
        if re.search(pattern,text):errors.append(name+': sensitive pattern (value omitted)');break
if errors:
    print('\n'.join(errors));sys.exit(1)
print('PASS: tracked publication files exclude local data and credential patterns')
