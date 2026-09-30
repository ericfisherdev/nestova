// Specs that need SMS (and email) to be OFFERED target a second server.
//
// The settings page only offers a notification channel when its sender is wired
// (NES-206), which needs NOTIFY_SMS_ENABLED / NOTIFY_EMAIL_ENABLED. The default
// checklist server leaves both off, so the specs that set a phone number, opt
// in, or choose "Text message" / "Email" target NESTOVA_SMS_BASE_URL instead: a
// second server on the same database started with both senders enabled. When
// that variable is unset those specs skip themselves.
//
// The second server must never reach a real provider. Start it with fake
// credentials and AWS_ENDPOINT_URL pointing at a closed local port, so a
// delivery attempt fails with "connection refused" and the dispatcher falls
// back to in-app:
//
//   env NOTIFY_SMS_ENABLED=true SMS_REGION=us-east-1 SMS_ORIGINATION_IDENTITY=+15555550199 \
//       SMS_ACCESS_KEY_ID=FAKEFAKEFAKEFAKEFAKE SMS_SECRET_ACCESS_KEY=fake SMS_RETRY_MAX_ATTEMPTS=1 \
//       NOTIFY_EMAIL_ENABLED=true SES_REGION=us-east-1 SES_FROM_ADDRESS=nestova@example.invalid \
//       SES_ACCESS_KEY_ID=FAKEFAKEFAKEFAKEFAKE SES_SECRET_ACCESS_KEY=fake \
//       AWS_ENDPOINT_URL=http://127.0.0.1:9 AWS_EC2_METADATA_DISABLED=true \
//       APP_ENV=dev PORT=<port> DATABASE_URL=<the slot's DSN> MEDIA_ROOT=<dir> CACHE_DIR=<dir> nestova-server
const SMS_BASE_URL = process.env.NESTOVA_SMS_BASE_URL || '';

const NO_SMS_SERVER = 'NESTOVA_SMS_BASE_URL is unset: these specs need a server started with '
  + 'NOTIFY_SMS_ENABLED=true and NOTIFY_EMAIL_ENABLED=true (see helpers-sms.js)';

module.exports = { SMS_BASE_URL, NO_SMS_SERVER };
