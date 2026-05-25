#!/usr/bin/env python3
"""ix REPL — persistent code execution engine for ix sandboxes."""
import sys
import traceback

def main():
    globals_dict = {"__builtins__": __builtins__}
    cell_count = 0

    while True:
        # Read until sentinel
        lines = []
        for line in sys.stdin:
            if line.rstrip('\n') == "__IX_END__":
                break
            lines.append(line)
        else:
            # stdin closed — daemon shut down
            break

        code = "".join(lines)
        if not code.strip():
            sys.stdout.write("__IX_RESULT__\n")
            sys.stdout.flush()
            continue

        cell_count += 1
        globals_dict[f"In_{cell_count}"] = code

        try:
            # Try as expression first (for auto-display)
            compiled = compile(code.strip(), "<ix>", "eval")
            result = eval(compiled, globals_dict)
            if result is not None:
                globals_dict[f"Out_{cell_count}"] = result
                print(repr(result))
        except SyntaxError:
            # Not an expression — exec as statements
            try:
                compiled = compile(code, "<ix>", "exec")
                exec(compiled, globals_dict)
            except Exception:
                traceback.print_exc()
        except Exception:
            traceback.print_exc()

        sys.stdout.write("__IX_RESULT__\n")
        sys.stdout.flush()

if __name__ == "__main__":
    main()
