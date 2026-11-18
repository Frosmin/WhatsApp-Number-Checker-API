import argparse
import csv
import json
import re
import time
from pathlib import Path
from tempfile import NamedTemporaryFile
from urllib.error import HTTPError
from urllib.request import ProxyHandler, build_opener

API = "http://127.0.0.1:8080/check"
HEADER = ["NRO_CUENTA", "estado"]
SAVE_INTERVAL = 25
urlopen = build_opener(ProxyHandler({})).open


def read_rows(path):
    with path.open(encoding="utf-8-sig", newline="") as file:
        return list(csv.reader(file))


def save_rows(path, rows):
    temp_path = None
    try:
        with NamedTemporaryFile(
            mode="w", dir=path.parent, encoding="utf-8", newline="", delete=False
        ) as file:
            temp_path = Path(file.name)
            writer = csv.writer(file, quoting=csv.QUOTE_ALL)
            writer.writerow(HEADER)
            writer.writerows(rows)
        temp_path.replace(path)
    finally:
        if temp_path is not None:
            temp_path.unlink(missing_ok=True)


def run(source, output, prefix, delay=1.0):
    if source.resolve() == output.resolve():
        raise ValueError("Indica archivos distintos para la entrada y la salida.")
    rows = read_rows(source)
    if not re.fullmatch(r"[1-9][0-9]{0,2}", prefix):
        raise ValueError("Indica el prefijo de país sin el signo +.")
    if not rows or rows[0] != HEADER[:1] or any(
        len(row) != 1 or not re.fullmatch(r"[0-9]{8}", row[0]) for row in rows[1:]
    ):
        raise ValueError("Se esperaba el CSV original con una columna NRO_CUENTA de ocho dígitos.")

    numbers = [row[0] for row in rows[1:]]
    saved = read_rows(output) if output.exists() else []
    saved_rows = saved[1:]
    if saved and (
        saved[0] not in (HEADER, ["NRO_CUENTA", "Estado WhatsApp"])
        or len(saved_rows) > len(numbers)
        or any(
            len(row) not in (1, 2)
            or row[0] != number
            or (len(row) == 2 and row[1] not in ("", "Activo", "Inactivo"))
            for row, number in zip(saved_rows, numbers)
        )
    ):
        raise ValueError("El archivo de resultados no coincide con este lote.")

    results = [[number, ""] for number in numbers]
    for result, row in zip(results, saved_rows):
        if len(row) == 2:
            result[1] = row[1]
    if not saved or saved[0] != HEADER or results != saved_rows:
        save_rows(output, results)

    completed = sum(bool(status) for _, status in results)
    unsaved = 0
    try:
        for row in results:
            number, status = row
            if status:
                continue
            international = prefix + number
            while True:
                try:
                    with urlopen(f"{API}?number={international}", timeout=15) as response:
                        result = json.load(response)
                    if (
                        not isinstance(result, dict)
                        or result.get("number") != "+" + international
                        or not isinstance(result.get("active"), bool)
                    ):
                        raise ValueError("La API devolvió una respuesta sin un estado confirmado.")
                    break
                except HTTPError as error:
                    if error.code == 429:
                        wait = error.headers.get("Retry-After")
                        wait_sec = int(wait) if wait and wait.isdigit() else 2
                        print(f"Pausa por límite de velocidad (HTTP 429). Esperando {wait_sec}s antes de reintentar...", flush=True)
                        time.sleep(wait_sec)
                        continue
                    print(f"Lote pausado. HTTP {error.code}. {completed}/{len(numbers)} confirmados.")
                    if error.code == 503:
                        print("Comprueba que el panel muestre una sesión de WhatsApp conectada.")
                    error.close()
                    return 1
                except (OSError, ValueError) as error:
                    print(f"Lote pausado. {completed}/{len(numbers)} confirmados. {error}")
                    return 1

            row[1] = "Activo" if result["active"] else "Inactivo"
            completed += 1
            unsaved += 1
            if unsaved == SAVE_INTERVAL:
                save_rows(output, results)
                unsaved = 0
            if completed % 100 == 0:
                print(f"{completed}/{len(numbers)} registros confirmados.", flush=True)

            if delay > 0:
                time.sleep(delay)
    finally:
        if unsaved:
            save_rows(output, results)

    print(f"Lote completo. {len(numbers)} registros guardados en {output}.")
    return 0


if __name__ == "__main__":
    parser = argparse.ArgumentParser(description="Consulta un lote en la API local y guarda cada resultado a 1 TPS.")
    parser.add_argument("entrada", type=Path)
    parser.add_argument("salida", type=Path)
    parser.add_argument("--prefijo", required=True, help="Código de país sin +, por ejemplo 591")
    parser.add_argument("--delay", type=float, default=1.0, help="Espera en segundos entre consultas (por defecto 1.0 = 1 TPS)")
    parser.add_argument("--tps", type=float, default=None, help="Transacciones por segundo (ej. 1 para 1 TPS; sobreescribe --delay)")
    args = parser.parse_args()
    delay = args.delay
    if args.tps is not None and args.tps > 0:
        delay = 1.0 / args.tps
    try:
        raise SystemExit(run(args.entrada, args.salida, args.prefijo, delay=delay))
    except (OSError, ValueError) as error:
        parser.exit(1, f"{error}\n")
    except KeyboardInterrupt:
        parser.exit(130, "\nLote interrumpido. Usa el mismo comando para reanudarlo.\n")
