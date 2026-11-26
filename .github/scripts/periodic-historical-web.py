"""Hosted same-checkout reproduction of the two archived PR #154 Web failures."""
import argparse
import hashlib
import importlib.util
import io
import json
import os
from pathlib import Path
import re
import shutil
import subprocess
import zipfile

REPO='PeterFile/ct0x'; RUN=44038474616; JOB=133886997987; SHA='3b7ff2f557df978e6c956487f9ba1875c56ae34f'; TREE='1557216981212620bd00837606aafc73a6361269'; EXPECTED_HEAD='49b6a939595dfaea9f3fe122082b8ca8eeadee5f'

def require(ok, reason):
    if not ok: raise ValueError(reason)

def get(path):
    return subprocess.check_output(['gh','api','--method','GET','repos/'+REPO+path])

def save(path,obj):
    path.write_text(json.dumps(obj,indent=2,ensure_ascii=False)+'\n')

parser=argparse.ArgumentParser(); parser.add_argument('phase',choices=['source','probe']);parser.add_argument('out',type=Path);args=parser.parse_args();out=args.out.resolve();out.mkdir(parents=True,exist_ok=True)
require(os.environ['FIXED_SHA']==SHA,'incorrect fixed revision')
subject=Path('subject').resolve(); source=Path(__file__).with_name('periodic-clients.py'); spec=importlib.util.spec_from_file_location('periodic_clients',source); clients=importlib.util.module_from_spec(spec);spec.loader.exec_module(clients)
require(subprocess.check_output(['git','-C',str(subject),'rev-parse','HEAD']).decode().strip()==SHA,'checkout SHA mismatch');require(subprocess.check_output(['git','-C',str(subject),'rev-parse','HEAD^{tree}']).decode().strip()==TREE,'checkout tree mismatch')
if args.phase=='source':
    run=json.loads(get(f'/actions/runs/{RUN}/attempts/1')); require((run['id'],run['run_attempt'],run['head_sha'],run['conclusion'],run['event'])==(RUN,1,EXPECTED_HEAD,'failure','pull_request'),'historical attempt mismatch')
    jobs=json.loads(get(f'/actions/runs/{RUN}/attempts/1/jobs?per_page=100'))['jobs']; rows=[j for j in jobs if j['id']==JOB and j['run_id']==RUN and j['run_attempt']==1 and j['conclusion']=='failure' and j['name']=='Web'];require(len(rows)==1,'historical failing leaf mismatch')
    archive=get(f'/actions/runs/{RUN}/attempts/1/logs');z=zipfile.ZipFile(io.BytesIO(archive)); match=[n for n in z.namelist() if re.fullmatch(r'[0-9]+_Web\.txt',n)];require(len(match)==1,'ambiguous run-attempt leaf')
    raw=z.read(match[0]);text=raw.decode();require(re.search(r'git log -1 --format=%H[^\n]*\n\S+ '+SHA+r'\s',text),'historical source missing immutable checkout');require('node: v22.23.5' in text and 'npm: 10.9.9' in text,'historical toolchain mismatch')
    found=clients.failures('web',text);require(len(found)==2 and {f['file'] for f in found}=={'web/src/App.test.tsx','web/src/RequestWork.test.tsx'},'historical failure signature mismatch')
    save(out/'source.json',{'run_id':RUN,'attempt':1,'job_id':JOB,'job_url':rows[0]['html_url'],'tested_sha':SHA,'tree':TREE,'api_head':run['head_sha'],'member':match[0],'zip_sha256':hashlib.sha256(archive).hexdigest(),'log_sha256':hashlib.sha256(raw).hexdigest(),'failures':found}); print('Verified original web failures and exact checkout:',SHA)
else:
    original=json.loads((out/'source.json').read_text());node=Path(shutil.which('node')).resolve();npm=Path(shutil.which('npm')).resolve();nv=subprocess.check_output([str(node),'--version']).decode().strip();pv=subprocess.check_output([str(node),str(npm),'--version']).decode().strip();require((nv,pv)==('v22.23.5','10.9.9'),'runner toolchain mismatch');os.environ.update(PERIODIC_WEB_NODE=str(node),PERIODIC_WEB_NPM=str(npm)); results=[]; ev=out/'raw';runner=clients.ClientProbe(subject,ev,timeout=360,budget=1500)
    try:
        for candidate in original['failures']:
            for repeat in (1,2):
                observed=runner(SHA,candidate); checked=clients.verify_probe(observed,candidate,ev,root=subject); result={'file':candidate['file'],'test':candidate['test'],'repeat':repeat,'state':observed.get('state'),'reconstructed':checked,'evidence':observed};results.append(result);print(candidate['file'],repeat,observed.get('state'),observed.get('reason',''),flush=True)
                save(out/'result.json',{'kind':'historical-web-replay','repo':REPO,'original':original,'node':nv,'npm':pv,'new_run':int(os.environ['GITHUB_RUN_ID']),'new_attempt':int(os.environ['GITHUB_RUN_ATTEMPT']),'control_sha':os.environ['GITHUB_SHA'],'module_sha256':hashlib.sha256(source.read_bytes()).hexdigest(),'results':results})
    finally: runner.close()
    require(all(r['state'] in ('pass','fail') and r['reconstructed'].get('state')==r['state'] for r in results),'incomplete or irreproducible evidence; inspect result.json')
