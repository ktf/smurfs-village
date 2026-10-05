"""Hidden check: `aliBuild build --max-load` parses to args.maxLoad (float, default None)."""
import shlex
import sys
from unittest import mock

sys.path.insert(0, ".")
import alibuild_helpers.args as A  # noqa: E402


def parse(cmd):
    with mock.patch("alibuild_helpers.utilities.getoutput", new=lambda c: "x86_64"), \
         mock.patch("alibuild_helpers.args.commands") as commands, \
         mock.patch.object(sys, "argv", ["alibuild"] + shlex.split(cmd)):
        commands.getstatusoutput.side_effect = lambda c: (0, "/usr/local/bin/docker")
        args, _ = A.doParseArgs()
        return vars(args)


assert parse("build zlib --max-load 2.5")["maxLoad"] == 2.5
assert isinstance(parse("build zlib --max-load 3")["maxLoad"], float)
assert parse("build zlib")["maxLoad"] is None
print("ok")
