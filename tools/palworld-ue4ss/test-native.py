#!/usr/bin/env python3
"""只针对 test-ue4ss.sh 新建的隔离空世界运行原生只读回归，不允许生产目录。"""
import pathlib
import subprocess
import sys
import time
import uuid

root = pathlib.Path(sys.argv[1]).resolve()
if root.parent != pathlib.Path('/home/games') or not root.name.startswith('gsp-ue4ss-test.'):
    raise SystemExit('拒绝非隔离目录')
queue = root / 'Pal/Binaries/Linux/gspanel-mod'
unit = 'gsp-ue4ss-test-' + root.name.split('.')[-1] + '.service'
for _ in range(100):
    if (queue / 'mod-alive.txt').exists():
        break
    if subprocess.run(['systemctl', 'is-active', '--quiet', unit]).returncode:
        raise SystemExit('测试实例启动失败：' + str(root / 'logs/console.log'))
    time.sleep(1)
else:
    raise SystemExit('mod 启动超时')


def command(verb, *args, expected_ok=True):
    if (queue / 'cmd.txt').exists():
        raise RuntimeError('隔离队列繁忙')
    request_id = uuid.uuid4().hex
    temp = queue / 'cmd.txt.tmp'
    temp.write_text('gsp2:' + request_id + '\n' + '\n'.join((verb,) + args) + '\n')
    temp.rename(queue / 'cmd.txt')
    for _ in range(75):
        try:
            result = (queue / 'res.txt').read_text()
            fields = result.split('\n', 2)
            if len(fields) == 3 and fields[0] == 'gsp2:' + request_id:
                if fields[1] != ('OK' if expected_ok else 'FAIL'):
                    raise RuntimeError(result)
                print(verb, *args, '=>', fields[1], fields[2].strip(), flush=True)
                return fields[2]
        except FileNotFoundError:
            pass
        time.sleep(.2)
    raise RuntimeError('原生命令超时/崩溃：' + verb)


command('probe', 'selftest')
command('probe', 'playercheck')
players = command('whojson').strip()
if players != '[]':
    raise RuntimeError('隔离空世界不应有玩家，拒绝继续')
command('probe', 'inventorycheck', '00000001000000000000000000000000', expected_ok=False)
command('give', '00000001000000000000000000000000', 'Wood', '1', expected_ok=False)
if subprocess.run(['systemctl', 'is-active', '--quiet', unit]).returncode:
    raise RuntimeError('测试后游戏已退出')
print('PASS: native CDO properties/UID, ProcessEvent/FName/UEnum, offline inventory/give rejection', flush=True)
