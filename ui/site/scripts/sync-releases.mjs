import { writeFile } from 'node:fs/promises';
import { fetchReleaseInfo } from '../lib/releases.ts';

const data = await fetchReleaseInfo(AbortSignal.timeout(15000));
await writeFile(new URL('../lib/release-data.json', import.meta.url), `${JSON.stringify(data, null, 2)}\n`);
console.log(`Verified Tutor MCP ${data.version}, Tutor ACP ${data.clientVersion}.`);
