"""Read-only historical CI command reproductions; retain both raw outputs."""
import argparse
import hashlib
import io
import json
import os
from pathlib import Path
import re
import shutil
import subprocess
import tempfile
import time
import zipfile

REPO='PeterFile/ct0x'
CASES={'vet':{'run':44390331394,'job':135115665712,'sha':'66e71aa66a1a58f52c8fa0b545d0aa135eab381a','file':'internal/platform/prepare_task_work.go','signature':'literal copies lock value from base: agentworkspace/internal/platform.memberToolRuntimeRun contains sync/atomic.Bool contains sync/atomic.noCopy','command':['go','vet','./...'],'step':'Run go vet ./...'},'python':{'run':43816639142,'job':133133787471,'sha':'85b488fa772dc1197241e730131aee5f2c8d365a','file':'.github/scripts/test-store-shard.py','signature':'AssertionError: False != True :','test':'test_remaining_package_exclusion_executes_actual_shell_without_go','command':['python3','.github/scripts/test-store-shard.py','-v'],'step':'Test Store shard gates'}}

def require(v,reason):
    if not v: raise ValueError(reason)

def save(path,obj):path.write_text(json.dumps(obj,indent=2,sort_keys=True,ensure_ascii=False)+'\n')

def h(raw):return hashlib.sha256(raw).hexdigest()

def api(path):return subprocess.check_output(['gh','api','--method','GET','repos/'+REPO+path])

def clean(text):return re.sub(r'\x1b\[[0-9;]*[A-Za-z]','',text)

parser=argparse.ArgumentParser();parser.add_argument('phase',choices=['source','repeat']);parser.add_argument('out',type=Path);args=parser.parse_args();out=args.out.resolve();out.mkdir(parents=True,exist_ok=True);c=CASES[os.environ['FIXED_CASE']];sha=os.environ['FIXED_SHA'];require(c['sha']==sha,'unexpected sha');subject=Path('subject').resolve();git=lambda *cmd:subprocess.check_output(['git','-C',str(subject),*cmd]).decode().strip();require(git('rev-parse','HEAD')==sha,'wrong checkout');tree=git('rev-parse','HEAD^{tree}');typ=os.environ['FIXED_CASE'];require('VERSION_ID="26.04"' in Path('/etc/os-release').read_text(), 'host OS differs from original Ubuntu 26.04');envref='https://github.com/actions/runner-images/blob/ubuntu26/20261111.196/images/ubuntu/Ubuntu2604-Readme.md'

def signature(text):
    text=clean(text)
    if typ=='vet': return [line for line in text.splitlines() if c['file'] in line and c['signature'] in line]
    rows=text.splitlines(); found=[]
    for i,row in enumerate(rows):
        if 'FAIL: '+c['test'] in row and "(case='valid')" in row:
            region=rows[i:i+17];assertions=[x for x in region if c['signature'] in x and not x.lstrip().startswith('self.assert')]
            if assertions and any(c['file'] in x for x in region):found.append([row,assertions[0]])
    return found

if args.phase=='source':
    run=json.loads(api(f'/actions/runs/{c["run"]}/attempts/1'));jobs=json.loads(api(f'/actions/runs/{c["run"]}/attempts/1/jobs?per_page=100'))['jobs'];job=next(j for j in jobs if j['id']==c['job']);require(run['event']=='pull_request' and run['conclusion']=='failure' and job['run_id']==c['run'] and job['run_attempt']==1 and job['conclusion']=='failure','authentic native failure absent');require(any(s['name']==c['step'] and s['conclusion']=='failure' for s in job['steps']),'wrong native failed step');canonical=json.loads(api('/git/commits/'+sha));require(canonical['tree']['sha']==tree,'git tree differs');archive=api(f'/actions/runs/{c["run"]}/attempts/1/logs');z=zipfile.ZipFile(io.BytesIO(archive));raw=z.read('0_Go.txt');txt=raw.decode();require(re.search(r'git log -1 --format=%H[^\n]*\n\S+ '+sha+r'\s',txt) and 'go version go1.26.0 linux/amd64' in txt and ' '.join(c['command']) in txt,'historical checkout, original command or Go version mismatch');matches=signature(txt);require(bool(matches),'original signature missing');
    settings={'kind':typ,'repo':REPO,'original_run_id':c['run'],'original_attempt':1,'original_job_id':c['job'],'original_url':job['html_url'],'original_step':c['step'],'original_argv':c['command'],'original_failures':matches,'run_head_sha':run['head_sha'],'sha':sha,'tree':tree,'archive_sha256':h(archive),'log_member':'0_Go.txt','log_sha256':h(raw),'original_image_url':next((l.split('Included Software: ',1)[1].strip() for l in txt.splitlines() if 'Included Software: ' in l),None)}
    if typ=='python': require(settings['original_image_url']==envref,'original Python image differs'); rd=json.loads(subprocess.check_output(['gh','api','repos/actions/runner-images/contents/images/ubuntu/Ubuntu2604-Readme.md?ref=ubuntu26%2F20261111.196']));import base64; image=base64.b64decode(rd['content']).decode();require('- Python 3.14.4' in image and 'Bash 5.3.9' in image,'original image interpreter evidence missing');settings['historical_python_proof']={'version':'3.14.4','image_blob':rd['sha'],'image_url':envref};
    save(out/'source.json',settings);print('Original verified',typ,sha,tree,job['html_url'])
else:
    original=json.loads((out/'source.json').read_text());require(original['tree']==tree,'source mismatch');actual_go=subprocess.check_output(['go','version']).decode().strip();require(actual_go=='go version go1.26.0 linux/amd64','Go differs');py=subprocess.check_output(['python3','--version']).decode().strip();bash=subprocess.check_output(['/bin/bash','--version']).decode().splitlines()[0];require(typ!='python' or py=='Python 3.14.4','Python minor/patch mismatch'); records=[]
    with tempfile.TemporaryDirectory() as temp:
        env={'PATH':os.environ['PATH'],'HOME':temp,'GOTOOLCHAIN':'local','GOWORK':'off','LANG':'C.UTF-8','LC_ALL':'C.UTF-8','GOPATH':str(Path(temp)/'go'),'GOCACHE':str(Path(temp)/'go-cache'),'GIT_CONFIG_NOSYSTEM':'1','GIT_TERMINAL_PROMPT':'0'}
        for n in (1,2):
            start=time.monotonic();r=subprocess.run(c['command'],cwd=subject,env=env,stdout=subprocess.PIPE,stderr=subprocess.STDOUT,timeout=660);elapsed=round(time.monotonic()-start,3);f=out/f'repeat-{n}.txt';f.write_bytes(r.stdout);matches=signature(r.stdout.decode(errors='replace'));state='fail' if r.returncode!=0 and matches else 'pass' if r.returncode==0 else 'unknown';records.append({'repeat':n,'code':r.returncode,'state':state,'signature':matches,'elapsed_seconds':elapsed,'raw_sha256':h(r.stdout),'path':f.name});save(out/'result.json',{'kind':'historical-command-replay','new_run_id':int(os.environ['GITHUB_RUN_ID']),'new_attempt':int(os.environ['GITHUB_RUN_ATTEMPT']),'control_sha':os.environ['GITHUB_SHA'],'original':original,'go':actual_go,'python':py,'bash':bash,'results':records});print(n,state,elapsed,flush=True)
    require(all(x['state'] in ('pass','fail') for x in records),'unrelated failure; inspect raw evidence')
