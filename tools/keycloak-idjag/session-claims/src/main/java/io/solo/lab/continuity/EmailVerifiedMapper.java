package io.solo.lab.continuity;

import java.util.List;

import com.fasterxml.jackson.databind.JsonNode;
import org.jboss.logging.Logger;
import org.keycloak.broker.oidc.OIDCIdentityProvider;
import org.keycloak.broker.provider.AbstractIdentityProviderMapper;
import org.keycloak.broker.provider.BrokeredIdentityContext;
import org.keycloak.broker.provider.IdentityBrokerException;
import org.keycloak.models.IdentityProviderMapperModel;
import org.keycloak.models.IdentityProviderSyncMode;
import org.keycloak.models.KeycloakSession;
import org.keycloak.models.RealmModel;
import org.keycloak.provider.ProviderConfigProperty;
import org.keycloak.representations.JsonWebToken;
import org.keycloak.services.messages.Messages;

/**
 * Refuses a sign-in unless the upstream says the user's email is verified:
 * {@code email_verified} true in its ID token or, when the ID token doesn't
 * carry the claim, in its userinfo response. Every sign-in through the IdP
 * passes it before the broker looks up or links an account, so linking by
 * email only ever trusts an address the upstream verified.
 *
 * <p>Keycloak's own essential-claim filter reads the ID token alone; an
 * upstream that returns email claims from userinfo only (Janssen/Gluu's
 * default) would be refused even with a verified email.
 */
public class EmailVerifiedMapper extends AbstractIdentityProviderMapper {
    public static final String PROVIDER_ID = "continuity-email-verified-idp-mapper";
    static final String CLAIM = "email_verified";
    private static final Logger LOG = Logger.getLogger(EmailVerifiedMapper.class);
    private static final String[] COMPATIBLE_PROVIDERS = {"oidc", "keycloak-oidc"};

    @Override public String getId() { return PROVIDER_ID; }
    @Override public String[] getCompatibleProviders() { return COMPATIBLE_PROVIDERS; }
    @Override public String getDisplayCategory() { return "Preprocessor"; }
    @Override public String getDisplayType() { return "Require a verified email"; }
    @Override public String getHelpText() {
        return "Refuses the sign-in unless email_verified is true in the upstream's ID token or, when the ID token doesn't carry it, its userinfo.";
    }
    @Override public List<ProviderConfigProperty> getConfigProperties() { return List.of(); }
    @Override public boolean supportsSyncMode(IdentityProviderSyncMode mode) { return true; }

    @Override
    public void preprocessFederatedIdentity(KeycloakSession session, RealmModel realm, IdentityProviderMapperModel model, BrokeredIdentityContext context) {
        String from = "ID token";
        Object v = null;
        if (context.getContextData().get(OIDCIdentityProvider.VALIDATED_ID_TOKEN) instanceof JsonWebToken idToken) {
            v = idToken.getOtherClaims().get(CLAIM);
        }
        if (v == null && context.getContextData().get(OIDCIdentityProvider.USER_INFO) instanceof JsonNode userinfo && userinfo.has(CLAIM)) {
            v = userinfo.get(CLAIM);
            from = "userinfo";
        }
        if (isTrue(v)) {
            LOG.debugf("%s verified (%s) for %s at %s", CLAIM, from, context.getBrokerUserId(), context.getIdpConfig().getAlias());
            return;
        }
        LOG.warnf("sign-in refused: %s at %s has no verified email (%s: %s)", context.getBrokerUserId(), context.getIdpConfig().getAlias(),
                v == null ? "neither ID token nor userinfo" : from, v);
        throw new IdentityBrokerException("the identity provider did not verify the user's email")
                .withMessageCode(Messages.IDENTITY_PROVIDER_UNMATCHED_ESSENTIAL_CLAIM_ERROR);
    }

    // true, or "true" (some upstreams send the claim as a string)
    static boolean isTrue(Object v) {
        if (v instanceof Boolean b) {
            return b;
        }
        if (v instanceof String s) {
            return s.equalsIgnoreCase("true");
        }
        if (v instanceof JsonNode n) {
            return n.isBoolean() ? n.asBoolean() : n.isTextual() && n.asText().equalsIgnoreCase("true");
        }
        return false;
    }
}
