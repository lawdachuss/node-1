#!/usr/bin/env python3
import os, sys, requests, time, logging, json, subprocess, re, hashlib
from pathlib import Path
from datetime import datetime, timezone

logging.basicConfig(level=logging.INFO, format='[UPLOAD] %(message)s')
log = logging.getLogger(__name__)

DB_FILE = '/database/recordings.json'

def load_db():
    if Path(DB_FILE).exists():
        with open(DB_FILE) as f:
            return json.load(f)
    return {"version": 2, "channels": {}}

def save_db(db):
    with open(DB_FILE, 'w') as f:
        json.dump(db, f, indent=2)

def extract_username(filename):
    if idx := filename.find('_2'):
        return filename[:idx]
    if idx := filename.find('_'):
        return filename[:idx]
    return 'unknown'

def fetch_channel_meta(username):
    try:
        r = requests.get(f'https://chaturbate.com/api/chatvideocontext/{username}/', timeout=15)
        if r.status_code != 200:
            return None
        data = r.json()
        title = data.get('room_title', '')
        tags = [t.strip('#') for t in re.findall(r'#\w+', title)]
        return {
            'room_title': title,
            'gender': data.get('broadcaster_gender', ''),
            'viewers': data.get('num_viewers', 0),
            'tags': tags,
        }
    except Exception as e:
        log.warning(f'Failed to fetch metadata for {username}: {e}')
        return None

def migrate_old_links(db):
    old_file = '/database/.uploaded_links.json'
    if not Path(old_file).exists():
        return db
    try:
        with open(old_file) as f:
            old = json.load(f)
        migrated = 0
        for filename, links in old.items():
            username = extract_username(filename)
            chan = db['channels'].setdefault(username, {'gender': '', 'recordings': []})
            exists = any(r['filename'] == filename for r in chan['recordings'])
            if not exists:
                chan['recordings'].append({
                    'filename': filename,
                    'timestamp': '',
                    'room_title': '',
                    'tags': [],
                    'viewers': 0,
                    'resolution': '',
                    'framerate': 0,
                    'links': links,
                })
                migrated += 1
        if migrated:
            log.info(f'Migrated {migrated} old uploads to recordings.json')
            save_db(db)
        Path(old_file).rename(Path(old_file).with_suffix('.json.bak'))
    except Exception as e:
        log.warning(f'Migration failed: {e}')
    return db

def get_embed_url(host_name, link):
    if not link:
        return ''
    if host_name == 'Streamtape' and '/v/' in link:
        code = link.split('/v/')[-1].split('/')[0]
        return f'https://streamtape.com/e/{code}' if code else ''
    if host_name == 'VoeSX':
        code = link.rsplit('/', 1)[-1]
        return f'https://voe.sx/e/{code}' if code else ''
    if host_name == 'Byse':
        code = link.rsplit('/', 1)[-1]
        return f'https://byse.sx/e/{code}' if code else ''
    return ''

def add_recording(db, filename, links, resolution='', framerate=0,
                  thumbnail_url='', sprite_url='', filesize=0, embed_url=''):
    username = extract_username(filename)
    chan = db['channels'].setdefault(username, {'gender': '', 'recordings': []})

    meta = fetch_channel_meta(username)
    if meta:
        if not chan['gender']:
            chan['gender'] = meta['gender']

    existing = [r for r in chan['recordings'] if r['filename'] == filename]
    if existing:
        existing[0]['links'].update(links)
        if meta:
            existing[0]['room_title'] = meta['room_title']
            existing[0]['tags'] = meta['tags']
            existing[0]['viewers'] = meta['viewers']
            existing[0]['resolution'] = resolution
            existing[0]['framerate'] = framerate
        if thumbnail_url:
            existing[0]['thumbnail_url'] = thumbnail_url
        if sprite_url:
            existing[0]['sprite_url'] = sprite_url
        if filesize:
            existing[0]['filesize'] = filesize
        if embed_url:
            existing[0]['embed_url'] = embed_url
        return

    timestamp = datetime.now(timezone.utc).strftime('%Y-%m-%dT%H:%M:%SZ')
    rec = {
        'filename': filename,
        'timestamp': timestamp,
        'room_title': meta['room_title'] if meta else '',
        'tags': meta['tags'] if meta else [],
        'viewers': meta['viewers'] if meta else 0,
        'resolution': resolution,
        'framerate': framerate,
        'links': links,
        'thumbnail_url': thumbnail_url,
        'sprite_url': sprite_url,
        'filesize': filesize,
        'embed_url': embed_url,
    }
    chan['recordings'].insert(0, rec)
    chan['recordings'] = sorted(chan['recordings'],
        key=lambda x: x.get('timestamp', ''), reverse=True)

def base_name(name):
    return Path(name).stem.replace('.video', '').replace('.audio', '')

def has_matching_audio(video_name, all_files):
    stem = Path(video_name).stem
    base = stem.replace('.video', '')
    for f in all_files:
        if Path(f).stem == base + '.audio':
            return f
    return None

def mux_pair(video_path, audio_path, output_path):
    log.info(f'Muxing {Path(video_path).name} + {Path(audio_path).name} -> {Path(output_path).name}')
    result = subprocess.run([
        'ffmpeg', '-y',
        '-i', video_path,
        '-i', audio_path,
        '-map', '0:v:0',
        '-map', '1:a:0',
        '-c', 'copy',
        '-copyts',
        '-shortest',
        '-avoid_negative_ts', 'make_zero',
        '-movflags', '+faststart',
        output_path
    ], capture_output=True, text=True, timeout=300)
    if result.returncode != 0:
        log.error(f'Mux failed: {result.stderr[-300:]}')
        return False
    if not Path(output_path).exists() or Path(output_path).stat().st_size == 0:
        log.error('Mux produced empty output')
        return False
    log.info(f'Mux OK: {Path(output_path).name}')
    return True

def upload_gofile(filepath):
    name = os.path.basename(filepath)
    key = os.environ.get('GOFILE_API_KEY', '')
    headers = {'Authorization': f'Bearer {key}'} if key else {}
    try:
        with open(filepath, 'rb') as fh:
            r = requests.post('https://upload.gofile.io/uploadfile',
                              headers=headers, files={'file': (name, fh)}, timeout=600)
        if r.status_code == 200 and r.json().get('status') == 'ok':
            link = r.json()['data'].get('downloadPage', '')
            if link:
                log.info(f'GoFile OK: {name} -> {link}')
                return link
    except Exception as e:
        log.warning(f'GoFile v2 FAIL: {e}')
    try:
        r = requests.get('https://api.gofile.io/servers', timeout=10)
        server = r.json()['data']['servers'][0]['name'] if r.status_code == 200 and r.json().get('status') == 'ok' else 'store1'
        url = f'https://{server}.gofile.io/contents/uploadfile'
        with open(filepath, 'rb') as fh:
            r = requests.post(url, headers=headers, files={'file': (name, fh)}, timeout=600)
        if r.status_code == 200 and r.json().get('status') == 'ok':
            link = r.json()['data'].get('downloadPage', '')
            if link:
                log.info(f'GoFile OK: {name} -> {link}')
                return link
    except Exception as e:
        log.warning(f'GoFile FAIL: {e}')
    return None

def upload_streamtape(filepath):
    name = os.path.basename(filepath)
    key = os.environ.get('STREAMTAPE_API_KEY', '')
    login = os.environ.get('STREAMTAPE_LOGIN', '')
    if not key or not login:
        log.info(f'Streamtape SKIP: missing STREAMTAPE_API_KEY or STREAMTAPE_LOGIN')
        return None
    max_attempts = 3
    for attempt in range(1, max_attempts + 1):
        try:
            sha256_hash = hashlib.sha256()
            with open(filepath, 'rb') as f:
                for byte_block in iter(lambda: f.read(4096), b""):
                    sha256_hash.update(byte_block)
            sha256 = sha256_hash.hexdigest()

            r = requests.get(f'https://api.streamtape.com/file/ul?login={login}&key={key}&sha256={sha256}', timeout=15)
            if r.status_code != 200:
                log.warning(f'Streamtape INIT FAIL (attempt {attempt}): HTTP {r.status_code} {r.text[:200]}')
                if attempt < max_attempts:
                    time.sleep(5)
                continue
            data = r.json()
            if data.get('status') != 200:
                log.warning(f'Streamtape INIT FAIL (attempt {attempt}): status={data.get("status")} {data.get("msg","")}')
                if attempt < max_attempts:
                    time.sleep(5)
                continue
            upload_url = data['result']['url']
            with open(filepath, 'rb') as fh:
                r = requests.post(upload_url, files={'file': (name, fh)}, timeout=(30, 600))
            if r.status_code == 200:
                result = r.json()
                link = result.get('result', {}).get('url', '') or result.get('url', '')
                if link:
                    log.info(f'Streamtape OK: {name} -> {link}')
                    return link
                log.warning(f'Streamtape UPLOAD OK but no URL in response: {str(result)[:200]}')
            else:
                log.warning(f'Streamtape UPLOAD FAIL (attempt {attempt}): HTTP {r.status_code} {r.text[:200]}')
        except Exception as e:
            log.warning(f'Streamtape FAIL (attempt {attempt}): {e}')
        if attempt < max_attempts:
            time.sleep(5)
    return None

def upload_voesx(filepath):
    name = os.path.basename(filepath)
    key = os.environ.get('VOESX_API_KEY', '')
    if not key:
        log.info(f'VoeSX SKIP: missing VOESX_API_KEY')
        return None
    max_attempts = 3
    for attempt in range(1, max_attempts + 1):
        try:
            r = requests.get(f'https://voe.sx/api/upload/server?key={key}', timeout=15)
            if r.status_code != 200:
                log.warning(f'VoeSX INIT FAIL (attempt {attempt}): HTTP {r.status_code} {r.text[:200]}')
                if attempt < max_attempts:
                    time.sleep(5)
                continue
            data = r.json()
            if not data.get('success'):
                log.warning(f'VoeSX INIT FAIL (attempt {attempt}): success=false, response={str(data)[:200]}')
                if attempt < max_attempts:
                    time.sleep(5)
                continue
            upload_url = data['result']

            with open(filepath, 'rb') as fh:
                r = requests.post(upload_url, data={'key': key}, files={'file': (name, fh)}, timeout=(30, 600))
            if r.status_code == 200:
                result = r.json()
                if result.get('success'):
                    file_code = result['file']['file_code']
                    link = result['file'].get('url', f'https://voe.sx/{file_code}')
                    log.info(f'VoeSX OK: {name} -> {link}')
                    return link
                log.warning(f'VoeSX UPLOAD OK but success=false: {str(result)[:200]}')
            else:
                log.warning(f'VoeSX UPLOAD FAIL (attempt {attempt}): HTTP {r.status_code} {r.text[:200]}')
        except Exception as e:
            log.warning(f'VoeSX FAIL (attempt {attempt}): {e}')
        if attempt < max_attempts:
            time.sleep(5)
    return None

def upload_sendcm(filepath):
    name = os.path.basename(filepath)
    key = os.environ.get('SENDCM_API_KEY', '')
    anon = not key
    max_attempts = 3
    for attempt in range(1, max_attempts + 1):
        try:
            if attempt > 1:
                backoff = (2 ** (attempt - 2)) * 5
                time.sleep(backoff)

            if anon:
                r = requests.get('https://send.now/api/upload/server', timeout=15)
            else:
                r = requests.get(f'https://send.now/api/upload/server?key={key}', timeout=15)
            if r.status_code != 200:
                log.warning(f'SendCM INIT FAIL (attempt {attempt}): HTTP {r.status_code} {r.text[:200]}')
                continue

            data = r.json()
            if not isinstance(data, dict):
                log.warning(f'SendCM INIT FAIL (attempt {attempt}): unexpected response type {type(data).__name__}: {str(data)[:200]}')
                continue

            if data.get('status') != 200:
                log.warning(f'SendCM INIT FAIL (attempt {attempt}): status={data.get("status")} {str(data)[:200]}')
                continue

            upload_url = data.get('result')
            if not upload_url:
                log.warning(f'SendCM INIT FAIL (attempt {attempt}): no upload URL in response: {str(data)[:200]}')
                continue

            with open(filepath, 'rb') as fh:
                r = requests.post(upload_url, data={'key': key}, files={'file': (name, fh)}, timeout=(30, 600))
            if r.status_code == 200:
                result = r.json()
                if isinstance(result, list):
                    if result and result[0].get('file_status') == 'OK':
                        file_code = result[0].get('file_code', '')
                        if file_code:
                            link = f'https://send.now/{file_code}'
                            log.info(f'SendCM OK: {name} -> {link}')
                            return link
                elif isinstance(result, dict):
                    if result.get('file_status') == 'OK':
                        file_code = result.get('file_code', '')
                        if file_code:
                            link = f'https://send.now/{file_code}'
                            log.info(f'SendCM OK: {name} -> {link}')
                            return link
                log.warning(f'SendCM UPLOAD OK but no URL: {str(result)[:200]}')
            else:
                log.warning(f'SendCM UPLOAD FAIL (attempt {attempt}): HTTP {r.status_code} {r.text[:200]}')
        except Exception as e:
            log.warning(f'SendCM FAIL (attempt {attempt}): {e}')
    return None

def upload_byse(filepath):
    name = os.path.basename(filepath)
    key = os.environ.get('BYSE_API_KEY', '')
    if not key:
        log.info(f'Byse SKIP: missing BYSE_API_KEY')
        return None
    max_attempts = 3
    for attempt in range(1, max_attempts + 1):
        try:
            r = requests.get(f'https://api.byse.sx/upload/server?key={key}', timeout=15)
            if r.status_code != 200:
                log.warning(f'Byse INIT FAIL (attempt {attempt}): HTTP {r.status_code} {r.text[:200]}')
                if attempt < max_attempts:
                    time.sleep(5)
                continue
            data = r.json()
            if data.get('status') != 200:
                log.warning(f'Byse INIT FAIL (attempt {attempt}): status={data.get("status")} {str(data)[:200]}')
                if attempt < max_attempts:
                    time.sleep(5)
                continue
            upload_url = data['result']

            with open(filepath, 'rb') as fh:
                r = requests.post(upload_url, data={'key': key}, files={'file': (name, fh)}, timeout=(30, 600))
            if r.status_code == 200:
                result = r.json()
                files = result.get('files', [])
                if files and files[0].get('status') == 'OK':
                    file_code = files[0].get('filecode', '')
                    if file_code:
                        link = f'https://byse.sx/d/{file_code}'
                        log.info(f'Byse OK: {name} -> {link}')
                        return link
                log.warning(f'Byse UPLOAD response: {str(result)[:200]}')
            else:
                log.warning(f'Byse UPLOAD FAIL (attempt {attempt}): HTTP {r.status_code} {r.text[:200]}')
        except Exception as e:
            log.warning(f'Byse FAIL (attempt {attempt}): {e}')
        if attempt < max_attempts:
            time.sleep(5)
    return None

def upload_file(filepath):
    name = os.path.basename(filepath)
    results = {}
    hosts = [
        ('GoFile', upload_gofile),
        ('Streamtape', upload_streamtape),
        ('SendCM', upload_sendcm),
        ('VoeSX', upload_voesx),
        ('Byse', upload_byse),
    ]
    for host_name, func in hosts:
        log.info(f'Uploading {name} to {host_name}...')
        link = func(filepath)
        if link:
            results[host_name] = link
        time.sleep(1)
    return results

def cleanup_sidecars(filepath):
    for ext in ('.thumb', '.sprite', '.clip'):
        sidecar = filepath + ext
        if os.path.isfile(sidecar):
            os.remove(sidecar)
            log.info(f'Deleted sidecar: {os.path.basename(sidecar)}')

def process_file(f, upload_dir, db):
    filepath = os.path.join(upload_dir, f)
    ext = os.path.splitext(f)[1].lower()
    if ext not in ('.ts', '.mp4', '.mkv'):
        return
    if not os.path.isfile(filepath):
        return

    all_files = os.listdir(upload_dir)

    if Path(f).stem.endswith('.audio'):
        os.remove(filepath)
        cleanup_sidecars(filepath)
        log.info(f'Deleted orphaned audio: {f}')
        return

    if Path(f).stem.endswith('.video'):
        audio_file = has_matching_audio(f, all_files)
        if audio_file:
            audio_path = os.path.join(upload_dir, audio_file)
            base = base_name(f)
            muxed_name = base + '.mp4'
            muxed_path = os.path.join(upload_dir, muxed_name)
            if mux_pair(filepath, audio_path, muxed_path):
                os.remove(filepath)
                cleanup_sidecars(filepath)
                os.remove(audio_path)
                cleanup_sidecars(audio_path)
                log.info(f'Deleted originals after mux: {f}, {audio_file}')
                process_file(muxed_name, upload_dir, db)
                return
        label = 'orphaned video (no audio)'
    else:
        label = 'merged file'

    username = extract_username(f)
    chan = db['channels'].get(username, {})
    if any(r['filename'] == f for r in chan.get('recordings', [])):
        return

    # Read sidecar URLs before they're deleted
    thumb_url = sprite_url = ''
    for sc_ext, target in [('.thumb', 'thumb_url'), ('.sprite', 'sprite_url')]:
        sc_path = filepath + sc_ext
        if os.path.isfile(sc_path):
            try:
                with open(sc_path) as sc_f:
                    val = sc_f.read().strip()
                    if val:
                        if sc_ext == '.thumb':
                            thumb_url = val
                        else:
                            sprite_url = val
            except Exception:
                pass

    filesize = os.path.getsize(filepath) if os.path.isfile(filepath) else 0

    log.info(f'Uploading {label}: {f}')
    results = upload_file(filepath)
    if results:
        embed_url = ''
        for host in ('Streamtape', 'VoeSX', 'Byse'):
            if host in results:
                eu = get_embed_url(host, results[host])
                if eu:
                    embed_url = eu
                    break
        add_recording(db, f, results, thumbnail_url=thumb_url,
                      sprite_url=sprite_url, filesize=filesize,
                      embed_url=embed_url)
        save_db(db)
        os.remove(filepath)
        cleanup_sidecars(filepath)
        log.info(f'Deleted: {f}')
        log.info(f'Saved to database with tags')

def watch():
    db = load_db()
    db = migrate_old_links(db)
    log.info('Starting upload watcher with tag extraction...')
    upload_dir = '/videos'
    while True:
        for f in sorted(os.listdir(upload_dir)):
            process_file(f, upload_dir, db)
        time.sleep(60)

if __name__ == '__main__':
    watch()
