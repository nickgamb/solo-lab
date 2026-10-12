package io.solo.lab.continuity;

import java.util.LinkedHashMap;
import java.util.Map;

import jakarta.ws.rs.Consumes;
import jakarta.ws.rs.POST;
import jakarta.ws.rs.Path;
import jakarta.ws.rs.PathParam;
import jakarta.ws.rs.Produces;
import jakarta.ws.rs.core.MediaType;
import jakarta.ws.rs.core.Response;
import org.jboss.logging.Logger;
import org.keycloak.broker.oidc.OIDCIdentityProvider;
import org.keycloak.events.Details;
import org.keycloak.events.EventBuilder;
import org.keycloak.events.EventType;
import org.keycloak.models.ClientModel;
import org.keycloak.models.KeycloakContext;
import org.keycloak.models.KeycloakSession;
import org.keycloak.models.RealmModel;
import org.keycloak.models.UserSessionModel;
import org.keycloak.protocol.oidc.OIDCAdvancedConfigWrapper;
import org.keycloak.protocol.oidc.utils.AuthorizeClientUtil;
import org.keycloak.representations.AccessToken;
import org.keycloak.services.cors.Cors;
import org.keycloak.services.managers.AuthenticationManager;
import org.keycloak.services.resource.RealmResourceProvider;

/**
 * The ID token an upstream IdP issued for the user's current sign-in, for a
 * requesting app that has that IdP vouch for the user in Cross App Access
 * (the IdP issues the ID-JAG from its own ID token).
 *
 * <p>{@code POST /realms/{realm}/continuity-upstream/{alias}/id-token}, form
 * {@code token=<the user's access token>}, client authentication required.
 * The gates are Identity Brokering API v2's: a confidential client with
 * "Allow retrieve external tokens" and the alias among its allowed IdPs, an
 * active access token issued to or for that client, and here also a session
 * that signed in through that IdP. v2's own endpoint returns the IdP's
 * access token only; the ID token stays in the session (the broker keeps it
 * for logout), and only that session's is ever returned.
 */
public class UpstreamIdTokenProvider implements RealmResourceProvider {
    static final String ID_TOKEN_TYPE = "urn:ietf:params:oauth:token-type:id_token";
    private static final Logger LOG = Logger.getLogger(UpstreamIdTokenProvider.class);
    private final KeycloakSession session;

    UpstreamIdTokenProvider(KeycloakSession session) {
        this.session = session;
    }

    @Override
    public Object getResource() {
        return this;
    }

    @Override
    public void close() {
    }

    @POST
    @Path("{alias}/id-token")
    @Consumes(MediaType.APPLICATION_FORM_URLENCODED)
    @Produces(MediaType.APPLICATION_JSON)
    public Response idToken(@PathParam("alias") String alias) {
        KeycloakContext ctx = session.getContext();
        RealmModel realm = ctx.getRealm();
        EventBuilder event = new EventBuilder(realm, session, ctx.getConnection())
                .event(EventType.IDENTITY_PROVIDER_RETRIEVE_TOKEN).detail(Details.IDENTITY_PROVIDER, alias);
        Cors cors = Cors.builder().auth().allowedMethods("POST");
        ClientModel client = AuthorizeClientUtil.authorizeClient(session, event, cors).getClient();
        event.client(client);
        if (client.isPublicClient()) {
            return refuse(event, Response.Status.FORBIDDEN, "unauthorized_client", "public clients may not read upstream tokens");
        }
        OIDCAdvancedConfigWrapper cfg = OIDCAdvancedConfigWrapper.fromClientModel(client);
        if (!cfg.getExternalTokenEnabled() || !cfg.getExternalAllowedIdentityProviders().contains(alias)) {
            return refuse(event, Response.Status.FORBIDDEN, "unauthorized_client", "this client may not read tokens from " + alias);
        }
        String token = ctx.getHttpRequest().getDecodedFormParameters().getFirst("token");
        if (token == null || token.isEmpty()) {
            return refuse(event, Response.Status.BAD_REQUEST, "invalid_request", "token is required");
        }
        AuthenticationManager.AuthResult auth = AuthenticationManager.verifyIdentityToken(session, realm, ctx.getUri(),
                ctx.getConnection(), true, true, null, false, token, ctx.getRequestHeaders(), v -> {
                });
        if (auth == null || auth.getSession() == null) {
            return refuse(event, Response.Status.BAD_REQUEST, "invalid_token", "the access token isn't active");
        }
        AccessToken at = auth.getToken();
        if (!client.getClientId().equals(at.getIssuedFor()) && !at.hasAudience(client.getClientId())) {
            return refuse(event, Response.Status.FORBIDDEN, "invalid_token", "the access token isn't for this client");
        }
        UserSessionModel us = auth.getSession();
        event.user(auth.getUser()).session(us);
        if (!alias.equals(us.getNote(Details.IDENTITY_PROVIDER))) {
            return refuse(event, Response.Status.BAD_REQUEST, "invalid_request", "this sign-in didn't come from " + alias);
        }
        String idToken = us.getNote(OIDCIdentityProvider.FEDERATED_ID_TOKEN);
        if (idToken == null || idToken.isEmpty()) {
            return refuse(event, Response.Status.BAD_REQUEST, "invalid_request", "no ID token from " + alias + " in this sign-in");
        }
        event.success();
        Map<String, Object> out = new LinkedHashMap<>();
        out.put("id_token", idToken);
        out.put("issued_token_type", ID_TOKEN_TYPE);
        out.put("token_type", "N_A");
        return cors.add(Response.ok(out, MediaType.APPLICATION_JSON_TYPE));
    }

    private Response refuse(EventBuilder event, Response.Status status, String error, String description) {
        event.error(error);
        LOG.debugf("upstream ID token refused: %s", description);
        Map<String, Object> out = new LinkedHashMap<>();
        out.put("error", error);
        out.put("error_description", description);
        return Response.status(status).entity(out).type(MediaType.APPLICATION_JSON_TYPE).build();
    }
}
