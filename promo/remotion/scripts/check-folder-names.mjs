// Имена <Folder> в Remotion валидируются только В РАНТАЙМЕ (validateFolderName:
// разрешены a-z, A-Z, 0-9 и дефис). Ни tsc, ни `remotion bundle` их не смотрят,
// поэтому кириллица в имени папки доживает до рендера и роняет его — так и
// случилось 2026-08-09. Эта проверка ловит то же самое за полсекунды и висит
// на `npm run typecheck`.
import {readFileSync} from 'node:fs';

const src = readFileSync(new URL('../src/Root.tsx', import.meta.url), 'utf8');
const names = [...src.matchAll(/<Folder\s+name="([^"]*)"/g)].map((m) => m[1]);
const bad = names.filter((n) => !/^[a-zA-Z0-9-]+$/.test(n));

if (bad.length > 0) {
  console.error(`Недопустимые имена папок (только a-zA-Z0-9-): ${bad.join(', ')}`);
  process.exit(1);
}
console.log(`Папки Studio: ${names.join(', ')} — имена валидны`);
