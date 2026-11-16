import RunkiteProvider from './runkite-provider.mjs';

if (process.argv.length !== 3) {
  console.error('Usage: node recover.mjs /absolute/private/receipt.json');
  process.exitCode = 2;
} else {
  try {
    const provider = new RunkiteProvider({ config: {
      profile: process.env.RUNKITE_PROMPTFOO_PROFILE,
      outputMode: process.env.RUNKITE_PROMPTFOO_OUTPUT_MODE,
    } });
    const result = await provider.observeReceipt(process.argv[2]);
    console.log(JSON.stringify(result, null, 2));
    if (result.error) process.exitCode = 1;
  } catch {
    console.error('Invalid Runkite provider configuration; no create was sent');
    process.exitCode = 2;
  }
}
