#!/usr/bin/env bash
# Format inspection corpus: images made with qemu-img/genisoimage and the
# original format_inspector's decision (as Glance's location.py makes it)
# for each disk_format. Then:
#   GLANCE_GO_FORMAT_CORPUS=DIR go test -run Corpus ./internal/glance/format/
# Uses the installed oslo.utils (python3-oslo.utils) or, with a second
# argument, that format_inspector.py (e.g. the one DevStack's venv has).
# Usage: scripts/glance_format_corpus.sh NEW_DIR [format_inspector.py]
set -euo pipefail
out=$1
[[ ! -e $out ]] || { echo "$out exists" >&2; exit 1; }
mkdir -p "$out/img" "$out/py/oslo_utils/imageutils"
py=$out/py
if [[ -n ${2:-} ]]; then
  touch "$py/oslo_utils/__init__.py" "$py/oslo_utils/imageutils/__init__.py"
  echo "def _(s): return s" > "$py/oslo_utils/_i18n.py"
  printf 'Ki=1024\nMi=1024*Ki\nGi=1024*Mi\n' > "$py/oslo_utils/units.py"
  printf 'NUM_KEY_SLOTS=8\nLUKS_SECTOR_SIZE=512\nclass LUKSHeader(dict): pass\n' > "$py/oslo_utils/imageutils/_luks.py"
  cp "$2" "$py/oslo_utils/imageutils/format_inspector.py"
  export PYTHONPATH=$py
fi
cd "$out/img"
qemu-img create -q -f raw raw.img 4M
qemu-img create -q -f qcow2 qcow2.img 64M
qemu-img create -q -f qcow2 -o compat=0.10 qcow2v2.img 64M
qemu-img create -q -f qcow2 -b qcow2.img -F qcow2 qcow2-backing.img 64M
qemu-img create -q -f qcow2 -o data_file="$out/img/datafile.raw" qcow2-datafile.img 16M
qemu-img create -q -f vmdk vmdk-sparse.img 32M
qemu-img create -q -f vmdk -o subformat=streamOptimized vmdk-stream.img 32M
qemu-img create -q -f vmdk -o subformat=monolithicFlat vmdk-flat.img 8M
qemu-img create -q -f vpc vhd.img 32M
qemu-img create -q -f vhdx vhdx.img 64M
qemu-img create -q -f vdi vdi.img 32M
qemu-img create -q -f qed qed.img 32M
mkdir -p isodir && echo hello > isodir/a.txt && genisoimage -quiet -o iso.img isodir
head -c 100000 /dev/urandom > random.img
for _ in $(seq 1 20); do printf 'just some text\nfor glance\n'; done > text.img
printf 'abc' > tiny.img
python3 - <<'PY'
import struct
mbr = bytearray(512); mbr[446:462] = struct.pack('<B3BB3BII', 0, 0, 2, 0, 0xEE, 0xff, 0xff, 0xff, 1, 0xffffffff); mbr[510:512] = b'\x55\xaa'
open('gpt.img', 'wb').write(bytes(mbr) + b'\0' * (1 << 20))
m2 = bytearray(512); m2[446:462] = struct.pack('<B3BB3BII', 0x80, 1, 1, 0, 0x83, 0xfe, 0xff, 0xff, 63, 2048); m2[510:512] = b'\x55\xaa'
open('mbr.img', 'wb').write(bytes(m2) + b'\0' * (1 << 20))
bad = bytearray(m2); bad[446] = 0x12
open('mbr-badboot.img', 'wb').write(bytes(bad) + b'\0' * 4096)
PY
cd "$out"
python3 - <<'PY'
import json, os
from oslo_utils.imageutils import format_inspector as fi
def decide(path, disk_format):
    data = fi.InspectWrapper(open(path, 'rb'))
    while data.read(65536):
        pass
    data.close()
    vs = 0
    try:
        mf = {str(i): i for i in data.formats}
        insp = mf['iso'] if mf.keys() == {'iso', 'gpt'} else data.format
        fmt = str(insp)
        if fmt == disk_format:
            vs = insp.virtual_size
        elif fmt == 'raw' and disk_format not in fi.ALL_FORMATS:
            pass
        elif disk_format == 'raw':
            vs = insp.actual_size
        else:
            raise fi.ImageFormatError('Image disk_format %s does not match stream data %s' % (disk_format, fmt))
        insp.safety_check()
    except fi.ImageFormatError as e:
        return 'err:' + str(e)
    except fi.SafetyCheckFailed as e:
        fatal = set(e.failures.keys())
        if disk_format in ('aki', 'ari'):
            fatal.discard('mbr')
        if insp.NAME == 'gpt' and not fatal:
            return 'vs:%d' % vs
        return 'err:Image failed safety checks'
    except Exception:
        return 'err:Image format inspection failed'
    return 'vs:%d' % vs
imgs = sorted(f for f in os.listdir('img') if f.endswith('.img') and not f.endswith('-flat.img'))
out = {i: {d: decide('img/' + i, d) for d in ['raw', 'qcow2', 'vmdk', 'vhd', 'vhdx', 'vdi', 'iso', 'ami', 'aki', 'ploop']} for i in imgs}
json.dump(out, open('expected.json', 'w'), indent=1, sort_keys=True)
print(len(imgs), 'images,', sum(len(v) for v in out.values()), 'decisions')
PY
