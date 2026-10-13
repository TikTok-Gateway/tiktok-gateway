"""Audit only Git-tracked publishable files; never read local secrets/state."""
import hashlib,pathlib,re,subprocess,sys
root=pathlib.Path(__file__).resolve().parents[1]
files=subprocess.check_output(['git','ls-files','-z'],cwd=root).decode().split('\0')
forbidden=['.secrets/','.state/','.tools/','bin/','.local/','config.json','internal/dashboard/','cmd/greeter/','cmd/audit/','deploy/']
patterns=[r'gh[pousr]_[A-Za-z0-9]{30,}',r'github_pat_[A-Za-z0-9_]{40,}',r'-----BEGIN (?:RSA |EC |OPENSSH )?PRIVATE KEY-----',r'/Users/'+r'[A-Za-z][^\s"\']*/']
# Reviewed generated logos; any replacement must be reviewed before updating its hash.
approved_images={
    'assets/brand/tiktok-gateway-logo-v1.png': '154d4449aaf2d5357fdb927c52e72dbba48f3bc82473f5c3d431161f3f97ac44',
    'assets/branding/tiktok-gateway-logo-v1.png': 'b97b1c419bb27b0d59336859abfa3945b35c169fafc7e6be9528d8ad06fd479e',
    'assets/branding/tiktok-gateway-logo-v2.png': '46ee76674a10aee0c8b779f7d2797a3a2a490e34cedc296f631f3eec71a35834',
    'assets/branding/tiktok-gateway-logo-v3.png': 'da1374da0bdef457639be7dfa848c120671dabaf4f1b2b806f0414080d1a4529',
}

errors=[]
for name in filter(None,files):
    if any(name==x or name.startswith(x) for x in forbidden):errors.append(name+': forbidden file');continue
    data=(root/name).read_bytes()
    if name in approved_images:
        if not data.startswith(b'\x89PNG\r\n\x1a\n') or hashlib.sha256(data).hexdigest()!=approved_images[name]:
            errors.append(name+': unreviewed image content')
        continue
    if b'\x00' in data:errors.append(name+': binary file');continue
    text=data.decode('utf-8')
    if name== 'scripts/audit_public.py':continue
    for pattern in patterns:
        if re.search(pattern,text):errors.append(name+': sensitive pattern (value omitted)');break
if errors:
    print('\n'.join(errors));sys.exit(1)
print('PASS: tracked publication files exclude local data and credential patterns')
